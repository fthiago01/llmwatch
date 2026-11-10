# llmwatch

Painel local que mostra em tempo real (WebSocket) o consumo de tokens e os limites dos planos de **Claude Code**, **OpenAI Codex** e **MiniMax**, lendo dados locais e APIs dos provedores.

---

## Recursos

- **Limites do plano** (janela de 5h e semanal: % usado, % restante e horário de reset) via *status line* do Claude Code, *app-server* do `codex` e API `token_plan/remains` da MiniMax.
- **Consumo de tokens por provider/modelo** nas janelas **5h**, **hoje**, **7d** e **30d**, a partir dos logs do Claude Code, do opencode e do Codex.
- **Bloco de 5h estimado** a partir dos tokens locais, com limite por provedor.
- **Gráfico das últimas 24h**, com tokens por hora separados por provider.
- **Página de admin** (`/admin`) para cadastrar providers e chaves de API, com teste de conexão por provider.
- **Login com senha** (cookie de sessão) e troca de senha obrigatória no primeiro acesso.
- **Binário único, sem dependências** — Go puro, sem CGO, sem runtime externo.

---

## Instalação

### Linux e macOS

```bash
curl -fsSL https://raw.githubusercontent.com/fthiago01/llmwatch/main/install.sh | bash
```

### Windows (PowerShell)

```powershell
irm https://raw.githubusercontent.com/fthiago01/llmwatch/main/install.ps1 | iex
```

### Forks privados

Se você mantiver um fork privado, o download direto do release não funciona.
Com o [GitHub CLI](https://cli.github.com) autenticado (`gh auth login`), aponte
o instalador para o seu repositório com `LLMWATCH_REPO` — ele usa
`gh release download` automaticamente quando o download direto falha:

```bash
gh api repos/<usuario>/llmwatch/contents/install.sh -H 'Accept: application/vnd.github.raw' | LLMWATCH_REPO=<usuario>/llmwatch bash
```

### O que o instalador faz

1. Baixa a release mais recente (ou a versão de `LLMWATCH_VERSION`) do GitHub.
2. Confere o **SHA-256** do arquivo contra o `checksums.txt` publicado na release; se divergir, a instalação é abortada.
3. Instala o binário em `~/.local/bin` (Linux/macOS) ou em `%LOCALAPPDATA%\llmwatch\bin` (Windows, que também adiciona o diretório ao `PATH` do usuário).
4. Cria o serviço para subir o servidor automaticamente:
   - **Linux**: serviço `systemd --user` (`llmwatch.service`);
   - **macOS**: `LaunchAgent` (`~/Library/LaunchAgents/io.github.llmwatch.plist`);
   - **Windows**: Tarefa Agendada que roda no logon, sem abrir janela.
5. Roda `llmwatch init`, que cria o banco, define a senha inicial e registra os providers detectados na máquina.
6. Mostra a senha inicial no final da saída.

No Linux, para o serviço rodar mesmo sem sessão aberta:

```bash
loginctl enable-linger $USER
```

### Variáveis aceitas pelo instalador

| Variável | Padrão | O que faz |
|---|---|---|
| `LLMWATCH_VERSION` | `latest` | Tag da release a instalar (ex.: `v0.1.0`) |
| `LLMWATCH_REPO` | `fthiago01/llmwatch` | Repositório de onde baixar a release |
| `LLMWATCH_ADDR` | `127.0.0.1:18123` | Endereço gravado no serviço |
| `LLMWATCH_INSTALL_DIR` | `$HOME/.local/bin` | Diretório do binário (só `install.sh`) |
| `LLMWATCH_NO_SERVICE` | — | Com `1`, não instala o serviço |
| `LLMWATCH_YES` | — | Com `1`, confirma as perguntas automaticamente (desinstalação) |

No Windows o diretório do binário é fixo (`%LOCALAPPDATA%\llmwatch\bin`); as
demais variáveis (`LLMWATCH_VERSION`, `LLMWATCH_REPO`, `LLMWATCH_ADDR`,
`LLMWATCH_NO_SERVICE`, `LLMWATCH_YES`) funcionam igual.

---

## Primeiro acesso

1. Abra **http://127.0.0.1:18123**.
2. Entre com a **senha inicial**. Ela é gerada na primeira execução, mostrada
   pelo instalador e gravada em `initial-password.txt` no diretório de dados
   (permissão `0600`).
3. Troque a senha: a política exige **no mínimo 12 caracteres**, com **pelo
   menos uma letra e um número** (e sem espaços no início ou no fim). A troca
   é obrigatória no primeiro acesso; depois dela o painel abre normalmente.

Esqueceu a senha? Gere uma nova (isso encerra todas as sessões):

```bash
llmwatch reset-password
```

A nova senha também é gravada em `initial-password.txt`.

> Se você definir `LLMWATCH_PASSWORD` antes da primeira execução, essa senha é
> usada em vez da gerada — e a troca não é exigida, mas continua sendo válida.

---

## App instalável e widget flutuante

O painel é um **PWA**: no Chrome ou Edge aparece o botão **Instalar app** no topo
(ou o ícone de instalar na barra de endereço). Ele abre em janela própria, com
ícone no menu iniciar/dock. A instalação só é oferecida em `http://127.0.0.1`,
`http://localhost` ou atrás de um proxy **HTTPS** — por IP de rede em HTTP puro o
navegador não permite.

O botão **Widget** abre uma janela flutuante **sempre acima dos outros apps**
(API Document Picture-in-Picture) com o "resta X%" e o reset de cada limite,
atualizada ao vivo. Limitações do navegador:

- disponível no Chrome/Edge para desktop (no Firefox e Safari o botão não aparece);
- precisa de um clique para abrir e fecha se a janela principal for fechada —
  minimize o painel em vez de fechá-lo;
- uma janela por vez; tamanho e posição podem ser ajustados.

O service worker guarda só ícones e o manifest: dados, API e páginas
autenticadas nunca ficam em cache.

---

## Configurando providers

Tudo é feito na página **`/admin`**: ela lista os providers, permite criar,
editar, habilitar/desabilitar, testar e apagar cada um.

Na **primeira execução** o llmwatch já registra automaticamente os providers
que encontra na máquina (habilitados, com a configuração padrão):

- diretório `~/.claude/projects` existe → provider de logs do Claude Code;
- `~/.local/share/opencode/opencode.db` existe → provider do opencode;
- `~/.codex/state_5.sqlite` existe → provider de logs do Codex;
- diretório `~/.claude` existe → provider dos limites do Claude;
- `codex` está no `PATH` → provider dos limites do Codex;
- `LLMWATCH_MINIMAX_KEY` está exportada → provider dos limites da MiniMax.

### Tipos de provider disponíveis

| id | O que mostra | Configuração |
|---|---|---|
| `claude-code-logs` | Consumo de tokens do Claude Code, por modelo | Diretório de transcripts — padrão `~/.claude/projects` |
| `opencode-db` | Consumo de tokens do opencode, por modelo | Banco de dados — padrão `~/.local/share/opencode/opencode.db` |
| `codex-db` | Consumo de tokens do Codex CLI, por modelo | Banco de dados — padrão `~/.codex/state_5.sqlite` |
| `claude-statusline` | Limites do plano do Claude Code (5h e 7d: % usado, restante e reset) | Arquivo da status line — padrão `<cache>/llmwatch/claude-statusline.json` |
| `minimax-plan` | Limites do M Plan da MiniMax (5h e semanal) | Chave da API (obrigatória) e URL — padrão `https://www.minimax.io/v1/token_plan/remains` |
| `codex-app-server` | Limites da conta do Codex (5h e 7d) | Executável do codex — padrão `codex` |

### Claude Code (limites)

Os limites do plano do Claude Code chegam pela *status line* do próprio Claude
Code. **O instalador configura isso automaticamente** (`llmwatch setup-claude`),
gravando no `~/.claude/settings.json` o caminho completo do binário — com backup
em `settings.json.llmwatch-bak` e sem mexer nas outras configurações. Depois,
reinicie o Claude Code.

- Já existe outra status line (ex.: ccusage)? Ela não é sobrescrita; para trocar,
  rode `llmwatch setup-claude --force`.
- Para desfazer: `llmwatch setup-claude --remove` (a desinstalação já faz isso).
- Para o instalador não mexer no Claude Code: `LLMWATCH_NO_CLAUDE_SETUP=1`.

O `llmwatch statusline` grava o JSON da status line no cache e imprime um
resumo (`Claude · 5h 42% · 7d 17%`) na própria linha de status.

> Os dados de limite **só atualizam enquanto o Claude Code está em uso** —
> nada é consultado na API da Anthropic.

### Codex (limites)

Rode `codex login` com a sua conta ChatGPT. O llmwatch consulta o
`codex app-server` localmente (nada sai da máquina) para ler os limites da
conta.

### MiniMax (limites)

Use a chave do **M Plan**, disponível em **Plan Details** em
`platform.minimax.io` — **não** a chave de *pay-as-you-go* (a API responde com
o código 2049 nesse caso). Cadastre a chave no provider `minimax-plan` na
página `/admin`. Para a detecção automática na primeira execução, exporte
`LLMWATCH_MINIMAX_KEY` (e opcionalmente `LLMWATCH_MINIMAX_URL`) antes de
instalar.

---

## Acesso remoto com segurança

Por padrão o servidor escuta **somente em `127.0.0.1`**: nada é exposto na
rede. Para acessar de fora, coloque um **proxy HTTPS** na frente e só então
mude o endereço de escuta:

```bash
LLMWATCH_ADDR=0.0.0.0:18123   # ou o IP da interface
LLMWATCH_TRUST_PROXY=1
```

Opções comuns: **Caddy**, **Nginx**, **Cloudflare Tunnel** ou **Tailscale**.

### Requisitos do proxy

- repassar o `Host` (ou `X-Forwarded-Host`) com o **hostname público** — é ele
  que o servidor usa para validar o `Origin` do WebSocket e do login;
- enviar `X-Forwarded-Proto: https` (define o cookie de sessão como `Secure`);
- enviar `X-Forwarded-For` e/ou `X-Real-IP` (usados no limite de tentativas de
  login);
- suportar **WebSocket em `/ws`**.

> **Nunca exponha o llmwatch sem HTTPS.** A senha e o cookie de sessão trafegam
> em texto claro se você publicar o servidor direto na internet.

### Exemplo — Caddy

O Caddy já repassa `Host`, `X-Forwarded-*` e suporta WebSocket:

```caddyfile
llmwatch.exemplo.com {
    reverse_proxy 127.0.0.1:18123
}
```

### Exemplo — Nginx

```nginx
map $http_upgrade $connection_upgrade {
    default upgrade;
    ''      close;
}

server {
    listen 443 ssl;
    server_name llmwatch.exemplo.com;

    location / {
        proxy_pass http://127.0.0.1:18123;
        proxy_http_version 1.1;

        proxy_set_header Upgrade    $http_upgrade;
        proxy_set_header Connection $connection_upgrade;

        proxy_set_header Host              $host;
        proxy_set_header X-Forwarded-Host  $host;
        proxy_set_header X-Forwarded-Proto https;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Real-IP         $remote_addr;

        proxy_read_timeout 3600s;
    }
}
```

### Onde colocar as variáveis

- **Linux**: em `~/.config/llmwatch/env` (o serviço systemd já lê esse arquivo
  como `EnvironmentFile`). Depois, reinicie o serviço:

  ```bash
  systemctl --user restart llmwatch
  ```

- **macOS**: o `~/.config/llmwatch/env` **não** é lido automaticamente — copie
  as variáveis para o `EnvironmentVariables` do plist
  (`~/Library/LaunchAgents/io.github.llmwatch.plist`) ou exporte-as antes de
  iniciar o serviço.

- **Windows**: defina as variáveis de ambiente do usuário (o instalador já
  grava `LLMWATCH_ADDR` no perfil do usuário quando ela é informada) e
  reinicie a tarefa agendada `llmwatch`.

Exemplo de `~/.config/llmwatch/env`:

```bash
LLMWATCH_ADDR=0.0.0.0:18123
LLMWATCH_TRUST_PROXY=1
```

---

## Variáveis de ambiente do servidor

| Variável | Padrão | Descrição |
|---|---|---|
| `LLMWATCH_ADDR` | `127.0.0.1:18123` | Endereço onde o servidor escuta |
| `LLMWATCH_TRUST_PROXY` | desligado | `1`, `true` ou `yes`: confia em `X-Forwarded-Host`, `X-Forwarded-Proto`, `X-Forwarded-For` e `X-Real-IP` |
| `LLMWATCH_DATA_DIR` | `<config dir>/llmwatch` | Diretório do banco e do `initial-password.txt` |
| `LLMWATCH_PASSWORD` | — | Senha usada na **primeira** inicialização; precisa cumprir a política (mín. 12 caracteres, com letra e número) |
| `LLMWATCH_UPDATE_CHECK` | ligado | `0` desativa a verificação de novas versões no GitHub |
| `LLMWATCH_LIMITS` | vazio | JSON com o limite de tokens do bloco de 5h, por provider. Ex.: `{"anthropic":2000000}` |

Sem `LLMWATCH_LIMITS` o bloco de 5h é estimado, mas sem percentual de uso.

Variáveis auxiliares usadas **apenas na detecção automática** da primeira
execução: `LLMWATCH_MINIMAX_KEY` e `LLMWATCH_MINIMAX_URL`.

---

## Onde ficam os dados

Diretório de dados (o `llmwatch.db` e o `initial-password.txt` ficam ali):

| Sistema | Caminho |
|---|---|
| Linux | `~/.config/llmwatch` |
| macOS | `~/Library/Application Support/llmwatch` |
| Windows | `%APPDATA%\llmwatch` |

Conteúdo do `llmwatch.db` (SQLite):

- `settings` — hash da senha em **bcrypt**, flag de troca obrigatória e flags internas;
- `sessions` — hash dos tokens de sessão (o token em claro nunca é gravado);
- `providers` — tipo, nome, habilitação e configuração de cada provider.

As **chaves de API ficam nesse arquivo**, com permissão `0600` (diretório
`0700`). Na interface elas **nunca** são exibidas em claro: o campo aparece
mascarado (`••••` + os 4 últimos caracteres) e, ao salvar, o valor em branco
mantém a chave que já estava gravada.

A *status line* do Claude Code é gravada no **cache** do sistema
(`~/.cache/llmwatch/claude-statusline.json` no Linux,
`~/Library/Caches/llmwatch/...` no macOS), com permissão `0600`.

---

## Comandos

| Comando | O que faz |
|---|---|
| `llmwatch serve` | Inicia o servidor HTTP + WebSocket (comando padrão) |
| `llmwatch init` | Prepara o banco, a senha inicial e os providers detectados |
| `llmwatch reset-password` | Gera uma nova senha e encerra todas as sessões |
| `llmwatch statusline` | Lê o JSON da status line do Claude Code da entrada padrão, grava no cache e imprime o resumo |
| `llmwatch version` | Mostra a versão do programa |

Use `llmwatch help` para a ajuda completa.

---

## Atualizar

Quando sai uma versão nova, o painel avisa com um toast no canto da tela, já com
o comando de atualização do seu sistema. O servidor consulta o último release
no GitHub ao iniciar e a cada 6 horas (desative com `LLMWATCH_UPDATE_CHECK=0`).

Para atualizar, rode o instalador de novo: ele baixa a release mais recente,
confere o checksum e substitui o binário, preservando senha, providers e chaves.

```bash
curl -fsSL https://raw.githubusercontent.com/fthiago01/llmwatch/main/install.sh | bash
```

```powershell
irm https://raw.githubusercontent.com/fthiago01/llmwatch/main/install.ps1 | iex
```

## Desinstalar

Linux e macOS (remove serviço e binário, **mantém os dados**):

```bash
curl -fsSL https://raw.githubusercontent.com/fthiago01/llmwatch/main/install.sh | bash -s -- --uninstall
```

Com `--purge`, apaga também os dados (banco, senha e providers):

```bash
curl -fsSL https://raw.githubusercontent.com/fthiago01/llmwatch/main/install.sh | bash -s -- --uninstall --purge
```

Windows:

```powershell
& ([scriptblock]::Create((irm https://raw.githubusercontent.com/fthiago01/llmwatch/main/install.ps1))) -Uninstall
```

Com `-Purge`, apaga também os dados:

```powershell
& ([scriptblock]::Create((irm https://raw.githubusercontent.com/fthiago01/llmwatch/main/install.ps1))) -Uninstall -Purge
```

---

## Desenvolvimento

Requer **Go 1.25+**.

```bash
go test -race ./...
go run .
```

O servidor em desenvolvimento sobe em `http://127.0.0.1:18123` (ajuste com
`LLMWATCH_ADDR`).

### Release

```bash
git tag vX.Y.Z
git push --tags
```

A tag dispara o workflow `.github/workflows/release.yml`, que roda os testes e
publica binários para **linux, darwin e windows**, nas arquiteturas **amd64 e
arm64**, junto com o `checksums.txt` usado pelos instaladores.
