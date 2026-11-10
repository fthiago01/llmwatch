#!/usr/bin/env bash
#
# llmwatch installer for Linux and macOS.
#
# Usage:
#   curl -fsSL <url>/install.sh | bash
#   curl -fsSL <url>/install.sh | bash -s -- --uninstall
#   curl -fsSL <url>/install.sh | bash -s -- --uninstall --purge
#
# Environment variables:
#   LLMWATCH_REPO         repository holding the releases (default fthiago01/llmwatch)
#   LLMWATCH_VERSION      release tag to install (default: latest)
#   LLMWATCH_INSTALL_DIR  directory of the binary (default $HOME/.local/bin)
#   LLMWATCH_ADDR         address of the server (default 127.0.0.1:18123)
#   LLMWATCH_NO_SERVICE    when set to 1, the service is not installed
#   LLMWATCH_NO_CLAUDE_SETUP
#                        when set to 1, the Claude Code status line is not
#                        configured automatically
#   LLMWATCH_YES          when set to 1, the confirmation is assumed

set -euo pipefail

REPO="${LLMWATCH_REPO:-fthiago01/llmwatch}"
VERSION="${LLMWATCH_VERSION:-latest}"
INSTALL_DIR="${LLMWATCH_INSTALL_DIR:-$HOME/.local/bin}"
ADDR="${LLMWATCH_ADDR:-127.0.0.1:18123}"

LAUNCH_LABEL="io.github.llmwatch"

# Global used to hand the path of the extracted binary between functions.
EXTRACTED_BIN=""
EXPECTED_SHA=""

# ---------------------------------------------------------------------------
# Output helpers. Colors are used only when stdout is a terminal.
# ---------------------------------------------------------------------------

if [ -t 1 ]; then
	C_RESET=$'\033[0m'
	C_BOLD=$'\033[1m'
	C_RED=$'\033[31m'
	C_GREEN=$'\033[32m'
	C_YELLOW=$'\033[33m'
	C_BLUE=$'\033[34m'
else
	C_RESET=""
	C_BOLD=""
	C_RED=""
	C_GREEN=""
	C_YELLOW=""
	C_BLUE=""
fi

info() { printf '%s\n' "$*"; }
step() { printf '%s==>%s %s\n' "$C_BLUE" "$C_RESET" "$*"; }
ok() { printf '%s ok %s%s\n' "$C_GREEN" "$C_RESET" "$*"; }
warn() { printf '%saviso:%s %s\n' "$C_YELLOW" "$C_RESET" "$*" >&2; }
err() {
	printf '%serro:%s %s\n' "$C_RED" "$C_RESET" "$*" >&2
	exit 1
}

# ---------------------------------------------------------------------------
# Platform detection.
# ---------------------------------------------------------------------------

detect_os() {
	local raw
	raw="$(uname -s)"
	case "$raw" in
	Linux) printf 'linux\n' ;;
	Darwin) printf 'darwin\n' ;;
	*)
		err "sistema operacional não suportado: $raw. No Windows use install.ps1."
		;;
	esac
}

detect_arch() {
	local raw
	raw="$(uname -m)"
	case "$raw" in
	x86_64 | amd64) printf 'amd64\n' ;;
	aarch64 | arm64) printf 'arm64\n' ;;
	*) err "arquitetura não suportada: $raw." ;;
	esac
}

# data_dir returns the directory holding the database and the initial password.
data_dir() {
	if [ "$(uname -s)" = "Darwin" ]; then
		printf '%s\n' "$HOME/Library/Application Support/llmwatch"
	else
		printf '%s\n' "$HOME/.config/llmwatch"
	fi
}

# ---------------------------------------------------------------------------
# Download helpers.
# ---------------------------------------------------------------------------

sha256_of() {
	local file="$1"
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$file" | awk '{print $1}'
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$file" | awk '{print $1}'
	else
		err "nenhuma ferramenta de sha256 encontrada (sha256sum ou shasum)."
	fi
}

# expected_sha looks up the checksum of the asset in the checksums file.
#
# The result is left in the global EXPECTED_SHA. An empty value means the
# asset is not listed.
expected_sha() {
	local asset="$1" sums="$2" line
	line="$(grep -E "[[:space:]]\*?${asset}\$" "$sums" | head -n 1 || true)"
	if [ -z "$line" ]; then
		EXPECTED_SHA=""
		return 0
	fi
	EXPECTED_SHA="$(printf '%s\n' "$line" | awk '{print $1}')"
}

release_url() {
	local asset="$1"
	if [ "$VERSION" = "latest" ]; then
		printf 'https://github.com/%s/releases/latest/download/%s\n' "$REPO" "$asset"
	else
		printf 'https://github.com/%s/releases/download/%s/%s\n' "$REPO" "$VERSION" "$asset"
	fi
}

# download fetches a file into the destination, using curl and, as a fallback
# for private repositories, the GitHub CLI.
download() {
	local url="$1" dest="$2" tag="$3"
	if command -v curl >/dev/null 2>&1 && curl -fsSL "$url" -o "$dest" 2>/dev/null; then
		return 0
	fi
	if command -v gh >/dev/null 2>&1 && gh auth status >/dev/null 2>&1; then
		info "baixando $tag com o gh (repositório privado)…"
		local gh_args=(release download)
		if [ "$VERSION" != "latest" ]; then
			gh_args+=("$VERSION")
		fi
		gh_args+=(-R "$REPO" -p "$(basename "$dest")" -D "$(dirname "$dest")")
		if gh "${gh_args[@]}" 2>/dev/null && [ -s "$dest" ]; then
			return 0
		fi
	fi
	rm -f "$dest"
	return 1
}

# fetch_release downloads the archive and the checksums file.
#
# The path of the extracted binary is left in the global EXTRACTED_BIN.
fetch_release() {
	local os="$1" arch="$2" tmp="$3"
	local asset="llmwatch_${os}_${arch}.tar.gz"
	local sums="$tmp/checksums.txt"
	local archive="$tmp/$asset"

	step "baixando $asset (${VERSION})…"
	if ! download "$(release_url "$asset")" "$archive" "$asset"; then
		err "não foi possível baixar $asset de $REPO. Se o repositório é privado, instale e autentique o GitHub CLI (gh auth login) com acesso ao repositório, ou defina LLMWATCH_REPO com um repositório público."
	fi
	if ! download "$(release_url "checksums.txt")" "$sums" "checksums.txt"; then
		err "não foi possível baixar checksums.txt de $REPO."
	fi

	step "verificando checksum…"
	local want got
	EXPECTED_SHA=""
	expected_sha "$asset" "$sums"
	want="$EXPECTED_SHA"
	if [ -z "$want" ]; then
		err "checksums.txt não contém o arquivo $asset. Download abortado."
	fi
	got="$(sha256_of "$archive")"
	if [ "$want" != "$got" ]; then
		err "checksum divergente para $asset (esperado $want, obtido $got). Download abortado."
	fi
	ok "checksum conferido."

	step "extraindo…"
	rm -rf "$tmp/extract"
	mkdir -p "$tmp/extract"
	tar -xzf "$archive" -C "$tmp/extract"
	local found
	found="$(find "$tmp/extract" -type f -name llmwatch -perm -u+x | head -n 1 || true)"
	if [ -z "$found" ]; then
		found="$(find "$tmp/extract" -type f -name llmwatch | head -n 1 || true)"
	fi
	if [ -z "$found" ]; then
		err "o arquivo $asset não contém o binário llmwatch."
	fi
	EXTRACTED_BIN="$found"
}

# ---------------------------------------------------------------------------
# Service management.
# ---------------------------------------------------------------------------

# build_path returns the PATH exported to the service.
build_path() {
	local path="/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
	local node codex
	if node="$(command -v node 2>/dev/null || true)"; then
		path="$(dirname "$node"):$path"
	fi
	if codex="$(command -v codex 2>/dev/null || true)"; then
		path="$(dirname "$codex"):$path"
	fi
	printf '%s\n' "$path"
}

has_systemd() {
	command -v systemctl >/dev/null 2>&1 || return 1
	systemctl --user show-environment >/dev/null 2>&1 || return 1
	return 0
}

install_systemd() {
	local bin="$1" path="$2"
	local unit_dir="$HOME/.config/systemd/user"
	local unit="$unit_dir/llmwatch.service"

	mkdir -p "$unit_dir"
	if [ -f "$unit" ]; then
		cp "$unit" "$unit.bak"
		warn "já existia um unit file em $unit; cópia salva em $unit.bak"
	fi
	cat >"$unit" <<EOF
[Unit]
Description=llmwatch - painel local de uso de LLMs
After=network.target

[Service]
Type=simple
ExecStart=$bin serve
Environment=LLMWATCH_ADDR=$ADDR
Environment=PATH=$path
EnvironmentFile=-%h/.config/llmwatch/env
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
EOF

	systemctl --user daemon-reload
	systemctl --user enable --now llmwatch.service
	systemctl --user restart llmwatch.service
}

install_launchd() {
	local bin="$1" path="$2"
	local agents="$HOME/Library/LaunchAgents"
	local plist="$agents/$LAUNCH_LABEL.plist"
	local logs="$HOME/Library/Logs"

	mkdir -p "$agents" "$logs"
	if [ -f "$plist" ]; then
		cp "$plist" "$plist.bak"
		warn "já existia um LaunchAgent em $plist; cópia salva em $plist.bak"
	fi
	cat >"$plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>$LAUNCH_LABEL</string>
  <key>ProgramArguments</key>
  <array>
    <string>$bin</string>
    <string>serve</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>LLMWATCH_ADDR</key>
    <string>$ADDR</string>
    <key>PATH</key>
    <string>$path</string>
  </dict>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>StandardOutPath</key>
  <string>$logs/llmwatch.log</string>
  <key>StandardErrorPath</key>
  <string>$logs/llmwatch.log</string>
</dict>
</plist>
EOF

	launchctl bootout "gui/$(id -u)" "$plist" 2>/dev/null || true
	launchctl bootstrap "gui/$(id -u)" "$plist"
}

install_service() {
	local bin="$1"
	local path
	path="$(build_path)"

	if [ "$(uname -s)" = "Darwin" ]; then
		step "instalando o LaunchAgent…"
		install_launchd "$bin" "$path"
		ok "LaunchAgent instalado em ~/Library/LaunchAgents/$LAUNCH_LABEL.plist"
		info "no macOS o arquivo ~/.config/llmwatch/env não é lido automaticamente: copie as variáveis para o plist (EnvironmentVariables) ou exporte-as antes de iniciar o serviço."
		return 0
	fi

	if has_systemd; then
		step "instalando o serviço systemd de usuário…"
		install_systemd "$bin" "$path"
		ok "serviço llmwatch.service instalado e iniciado."
		return 0
	fi

	warn "systemd de usuário indisponível: o serviço não foi instalado."
	info "para rodar manualmente:"
	info "  nohup '$bin' serve >/dev/null 2>&1 &"
	return 0
}

# stop_service stops a running service without removing its definition.
#
# It is used before the binary is replaced, so the old process does not keep
# running with a deleted executable.
stop_service() {
	if [ "$(uname -s)" = "Darwin" ]; then
		local plist="$HOME/Library/LaunchAgents/$LAUNCH_LABEL.plist"
		if [ -f "$plist" ]; then
			launchctl bootout "gui/$(id -u)" "$plist" 2>/dev/null || true
			ok "serviço parado."
		fi
		return 0
	fi

	if command -v systemctl >/dev/null 2>&1; then
		if systemctl --user is-active --quiet llmwatch.service 2>/dev/null; then
			systemctl --user stop llmwatch.service >/dev/null 2>&1 || true
			ok "serviço parado."
		fi
	fi
}

remove_service() {
	if [ "$(uname -s)" = "Darwin" ]; then
		local plist="$HOME/Library/LaunchAgents/$LAUNCH_LABEL.plist"
		if [ -f "$plist" ]; then
			launchctl bootout "gui/$(id -u)" "$plist" 2>/dev/null || true
			rm -f "$plist" "$plist.bak"
			ok "LaunchAgent removido."
		fi
		return 0
	fi

	if command -v systemctl >/dev/null 2>&1; then
		systemctl --user stop llmwatch.service >/dev/null 2>&1 || true
		systemctl --user disable llmwatch.service >/dev/null 2>&1 || true
		rm -f "$HOME/.config/systemd/user/llmwatch.service" \
			"$HOME/.config/systemd/user/llmwatch.service.bak"
		systemctl --user daemon-reload >/dev/null 2>&1 || true
		ok "serviço systemd removido."
	fi
}

# ---------------------------------------------------------------------------
# Claude Code status line.
# ---------------------------------------------------------------------------

# CLAUDE_SETUP_OUTPUT holds the output of the last "setup-claude" run.
#
# It is empty when the setup was not executed at all.
CLAUDE_SETUP_OUTPUT=""

# CLAUDE_SETUP_RAN tells whether the setup was attempted.
CLAUDE_SETUP_RAN=0

# setup_claude configures the status line of the Claude Code, pointing it to
# the absolute path of the binary.
#
# A failure is only a warning: the installation itself must not be aborted
# because of the status line. The output is kept in CLAUDE_SETUP_OUTPUT so the
# summary can show it.
setup_claude() {
	local bin="$1"
	local out

	CLAUDE_SETUP_RAN=1

	if out="$("$bin" setup-claude 2>&1)"; then
		CLAUDE_SETUP_OUTPUT="$out"
		return 0
	fi

	CLAUDE_SETUP_OUTPUT=""
	warn "não foi possível configurar a status line do Claude Code."
	info "rode '$bin setup-claude' depois de terminar a instalação."
	return 0
}

# remove_claude_statusline removes the status line entry from the settings of
# the Claude Code.
#
# It is called before the binary is removed, so the command is still
# available. Failures are ignored with a warning.
remove_claude_statusline() {
	local bin="$1"
	local out

	if out="$("$bin" setup-claude --remove 2>&1)"; then
		if [ -n "$out" ]; then
			printf '%s\n' "$out"
		fi
		return 0
	fi

	warn "não foi possível remover a status line do Claude Code."
	return 0
}

# ---------------------------------------------------------------------------
# Uninstall.
# ---------------------------------------------------------------------------

confirm() {
	local prompt="$1"
	if [ "${LLMWATCH_YES:-}" = "1" ]; then
		return 0
	fi
	local answer=""
	local tty_ok=0
	if (: >/dev/tty) 2>/dev/null; then
		tty_ok=1
	fi
	if [ "$tty_ok" = "1" ]; then
		printf '%s [s/N] ' "$prompt" >/dev/tty || true
		read -r answer </dev/tty || answer=""
	elif [ -t 0 ]; then
		printf '%s [s/N] ' "$prompt"
		read -r answer || answer=""
	else
		err "confirmação necessária e nenhum terminal disponível. Use LLMWATCH_YES=1 para confirmar automaticamente."
	fi
	case "$answer" in
	s | S | sim | y | Y) return 0 ;;
	*) return 1 ;;
	esac
}

uninstall() {
	local purge="$1"
	local bin="$INSTALL_DIR/llmwatch"

	step "removendo o serviço…"
	remove_service

	if [ -e "$bin" ]; then
		step "removendo a status line do Claude Code…"
		remove_claude_statusline "$bin"

		rm -f "$bin"
		ok "binário removido: $bin"
	else
		info "binário não encontrado em $bin"
	fi

	local dir
	dir="$(data_dir)"
	if [ "$purge" = "1" ]; then
		if confirm "Apagar também os dados em $dir?"; then
			rm -rf "$dir"
			ok "dados removidos: $dir"
		else
			info "dados mantidos em $dir"
		fi
	else
		info "dados mantidos em $dir (use --purge para apagá-los)."
	fi
	ok "llmwatch removido."
}

# ---------------------------------------------------------------------------
# Main.
# ---------------------------------------------------------------------------

main() {
	local uninstall_requested=0
	local purge_requested=0

	while [ $# -gt 0 ]; do
		case "$1" in
		--uninstall | -u) uninstall_requested=1 ;;
		--purge | -p) purge_requested=1 ;;
		-h | --help)
			info "uso: install.sh [--uninstall] [--purge]"
			info ""
			info "  --uninstall  para e remove o serviço e o binário (mantém os dados)"
			info "  --purge      com --uninstall, apaga também os dados"
			info ""
			info "a versão pode ser escolhida com LLMWATCH_VERSION=v0.1.0"
			return 0
			;;
		*) err "opção desconhecida: $1" ;;
		esac
		shift
	done

	if [ "$uninstall_requested" = "1" ]; then
		uninstall "$purge_requested"
		return 0
	fi

	local os arch tmp bin init_output
	os="$(detect_os)"
	arch="$(detect_arch)"

	if [ "${LLMWATCH_NO_SERVICE:-}" = "1" ]; then
		info "LLMWATCH_NO_SERVICE=1: o serviço não será instalado."
	fi

	tmp="$(mktemp -d)"
	# shellcheck disable=SC2064
	trap "rm -rf '$tmp'" EXIT INT TERM

	local extracted=""
	EXTRACTED_BIN=""
	fetch_release "$os" "$arch" "$tmp"
	extracted="$EXTRACTED_BIN"

	bin="$INSTALL_DIR/llmwatch"
	mkdir -p "$INSTALL_DIR"

	if [ -e "$bin" ]; then
		step "parando o serviço existente antes de substituir o binário…"
		stop_service
	fi

	step "instalando em $bin…"
	if command -v install >/dev/null 2>&1; then
		install -m 0755 "$extracted" "$bin"
	else
		cp "$extracted" "$bin"
		chmod 0755 "$bin"
	fi
	ok "binário instalado."

	step "inicializando o banco de dados…"
	init_output="$("$bin" init 2>&1 || true)"
	if [ -n "$init_output" ]; then
		printf '%s\n' "$init_output"
	fi

	if [ "${LLMWATCH_NO_CLAUDE_SETUP:-}" != "1" ]; then
		step "configurando a status line do Claude Code…"
		setup_claude "$bin"
	else
		info "LLMWATCH_NO_CLAUDE_SETUP=1: a status line do Claude Code não será configurada."
	fi

	if [ "${LLMWATCH_NO_SERVICE:-}" != "1" ]; then
		install_service "$bin"
	fi

	# -------------------------------------------------------------------------
	# Summary.
	# -------------------------------------------------------------------------

	local version_output
	version_output="$("$bin" version 2>&1 || true)"

	printf '\n%s%s%s\n' "$C_BOLD" "llmwatch instalado." "$C_RESET"
	info "versão: ${version_output:-desconhecida}"
	info "url: http://$ADDR"

	if [ -n "$init_output" ]; then
		printf '\n%s%s%s\n' "$C_BOLD" "saída do init:" "$C_RESET"
		printf '%s\n' "$init_output"
		if printf '%s' "$init_output" | grep -q "Senha inicial"; then
			warn "troque a senha inicial no primeiro acesso."
		fi
	fi

	printf '\n%s%s%s\n' "$C_BOLD" "status line do Claude Code:" "$C_RESET"
	if [ "$CLAUDE_SETUP_RAN" = "1" ] && [ -n "$CLAUDE_SETUP_OUTPUT" ]; then
		printf '%s\n' "$CLAUDE_SETUP_OUTPUT"
	else
		info "rode: llmwatch setup-claude"
	fi
	info "reinicie o Claude Code para a status line valer."

	if [ "$(uname -s)" = "Linux" ] && has_systemd; then
		printf '\n%s%s%s\n' "$C_BOLD" "dica:" "$C_RESET"
		info "para rodar sem sessão aberta: loginctl enable-linger $USER"
	fi

	case ":$PATH:" in
	*":$INSTALL_DIR:"*) ;;
	*)
		warn "$INSTALL_DIR não está no PATH."
		info "adicione ao seu shell rc (ex.: ~/.bashrc ou ~/.zshrc):"
		info "  export PATH=\"$INSTALL_DIR:\$PATH\""
		;;
	esac
}

main "$@"
