<#
.SYNOPSIS
    Instalador do llmwatch para Windows.

.DESCRIPTION
    Baixa o binario do llmwatch a partir das releases do GitHub, valida o
    checksum SHA256, instala em %LOCALAPPDATA%\llmwatch\bin, adiciona o
    diretorio ao PATH do usuario e registra uma tarefa agendada que sobe o
    servidor em segundo plano a cada login, sem abrir janela.

.PARAMETER Uninstall
    Remove a instalacao: tarefa agendada, processo, binario e o diretorio
    do PATH. Mantem os dados em %APPDATA%\llmwatch.
    Tambem pode ser ativado com $env:LLMWATCH_UNINSTALL='1' (irm | iex).

.PARAMETER Purge
    Com -Uninstall, tambem apaga os dados em %APPDATA%\llmwatch. Pede
    confirmacao, a menos que $env:LLMWATCH_YES='1'.
    Tambem pode ser ativado com $env:LLMWATCH_PURGE='1'.

.EXAMPLE
    irm https://raw.githubusercontent.com/fthiago01/llmwatch/main/install.ps1 | iex

.EXAMPLE
    & ([scriptblock]::Create((irm https://raw.githubusercontent.com/fthiago01/llmwatch/main/install.ps1))) -Uninstall
#>
param(
    [switch]$Uninstall,
    [switch]$Purge
)

# param() e valido no inicio do texto passado a Invoke-Expression.
# O escopo filho isola funcoes, variaveis e preferencias da sessao do usuario.
& {
param([switch]$Uninstall, [switch]$Purge)

$ErrorActionPreference = 'Stop'

# --- Constantes -------------------------------------------------------------

$DefaultRepo    = 'fthiago01/llmwatch'
$TaskName       = 'llmwatch'
$InstallDirName = 'llmwatch'
$DefaultAddr    = '127.0.0.1:18123'
$TaskUser       = "$env:USERDOMAIN\$env:USERNAME"

# --- Utilitarios -------------------------------------------------------------

function Write-Step {
    param([string]$Message)
    Write-Host "==> $Message" -ForegroundColor Cyan
}

function Write-Info {
    param([string]$Message)
    Write-Host "    $Message" -ForegroundColor Gray
}

function Write-Ok {
    param([string]$Message)
    Write-Host "    $Message" -ForegroundColor Green
}

function Write-Warn {
    param([string]$Message)
    Write-Host "    $Message" -ForegroundColor Yellow
}

function Write-Err {
    param([string]$Message)
    Write-Host "ERRO: $Message" -ForegroundColor Red
}

# Test-Dependency verifies that a command is available in the PATH.
function Test-Dependency {
    param([string]$Name)
    return $null -ne (Get-Command $Name -ErrorAction SilentlyContinue)
}

# No 5.1, stderr redirecionado gera ErrorRecord e pode lancar com Stop.
# No 7, nao herdar a politica opcional de erros de comandos nativos.
function Invoke-NativeCommand {
    param([string]$Command, [string[]]$Arguments = @())
    if ([System.IO.Path]::IsPathRooted($Command)) {
        if (-not [System.IO.File]::Exists($Command)) {
            throw "Executavel nao encontrado: '$Command'."
        }
        $applicationPath = $Command
    } else {
        $application = Get-Command -Name $Command -CommandType Application -ErrorAction Stop |
            Select-Object -First 1
        $applicationPath = $application.Source
    }
    $ErrorActionPreference = 'Continue'
    $PSNativeCommandUseErrorActionPreference = $false
    $LASTEXITCODE = $null
    $output = (& $applicationPath @Arguments 2>&1 | Out-String).Trim()
    $code = $LASTEXITCODE
    if ($null -eq $code) {
        throw "Nao foi possivel executar '$Command'. $output"
    }
    return [pscustomobject]@{ Output = $output; ExitCode = $code }
}

# Get-Architecture maps the processor architecture to the name used by the
# release assets (amd64 or arm64).
#
# On a 64-bit system running the 32-bit PowerShell, PROCESSOR_ARCHITECTURE
# reports x86 and PROCESSOR_ARCHITEW6432 reports the real architecture.
function Get-Architecture {
    $arch = $env:PROCESSOR_ARCHITECTURE
    $native = $env:PROCESSOR_ARCHITEW6432
    if (-not [string]::IsNullOrWhiteSpace($native)) {
        $arch = $native
    }
    switch -Regex ($arch) {
        '^(AMD64|x64)$'          { return 'amd64' }
        '^(ARM64|ARM64EC)$'      { return 'arm64' }
        default {
            throw "Arquitetura nao suportada: '$arch'. Use Windows x64 (AMD64) ou ARM64."
        }
    }
}

# Get-Repo returns the repository that holds the releases.
function Get-Repo {
    if (-not [string]::IsNullOrWhiteSpace($env:LLMWATCH_REPO)) {
        return $env:LLMWATCH_REPO.Trim()
    }
    return $DefaultRepo
}

# Get-Version returns the requested version or 'latest'.
function Get-Version {
    if (-not [string]::IsNullOrWhiteSpace($env:LLMWATCH_VERSION)) {
        return $env:LLMWATCH_VERSION.Trim()
    }
    return 'latest'
}

# Get-InstallDir returns the directory holding the binary and the helper
# files (%LOCALAPPDATA%\llmwatch).
function Get-InstallDir {
    if ([string]::IsNullOrWhiteSpace($env:LOCALAPPDATA)) {
        throw "Variavel de ambiente LOCALAPPDATA nao definida."
    }
    return (Join-Path $env:LOCALAPPDATA $InstallDirName)
}

# Get-BinDir returns the directory holding the executable.
function Get-BinDir {
    return (Join-Path (Get-InstallDir) 'bin')
}

# Get-DataDir returns the directory holding the database and the password
# file (%APPDATA%\llmwatch).
function Get-DataDir {
    if ([string]::IsNullOrWhiteSpace($env:APPDATA)) {
        throw "Variavel de ambiente APPDATA nao definida."
    }
    return (Join-Path $env:APPDATA $InstallDirName)
}

# Get-Address returns the address the server listens on.
function Get-Address {
    if (-not [string]::IsNullOrWhiteSpace($env:LLMWATCH_ADDR)) {
        return $env:LLMWATCH_ADDR.Trim()
    }
    return $DefaultAddr
}

# Ler o valor bruto evita expandir e substituir entradas como %USERPROFILE%.
function Get-UserPathValue {
    $key = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Environment')
    if ($null -eq $key) { return '' }
    try {
        return [string]$key.GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
    } finally {
        $key.Close()
    }
}

function Set-UserPathValue {
    param([string]$Value)
    # Preservar REG_SZ/REG_EXPAND_SZ; um PATH novo pode usar referencias %VAR%.
    $key = [Microsoft.Win32.Registry]::CurrentUser.CreateSubKey('Environment')
    try {
        $kind = [Microsoft.Win32.RegistryValueKind]::ExpandString
        if ($key.GetValueNames() -contains 'Path') {
            $kind = $key.GetValueKind('Path')
        }
        if ($kind -ne [Microsoft.Win32.RegistryValueKind]::String -and
            $kind -ne [Microsoft.Win32.RegistryValueKind]::ExpandString) {
            throw 'O PATH do usuario no Registro nao e um valor de texto.'
        }
        $key.SetValue('Path', $Value, $kind)
    } finally {
        $key.Close()
    }
    # Notificar o Explorer sem usar setx (que pode truncar o PATH).
    if ($null -eq ('LlmwatchInstaller.EnvironmentNotification' -as [type])) {
        Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;
namespace LlmwatchInstaller {
    public static class EnvironmentNotification {
        [DllImport("user32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
        public static extern IntPtr SendMessageTimeout(IntPtr window, uint message,
            UIntPtr wParam, string lParam, uint flags, uint timeout, out UIntPtr result);
    }
}
'@
    }
    $result = [UIntPtr]::Zero
    [void][LlmwatchInstaller.EnvironmentNotification]::SendMessageTimeout(
        [IntPtr]65535, 0x001A, [UIntPtr]::Zero, 'Environment', 2, 1000, [ref]$result)
}

# Comparar sem mudar o texto, a ordem ou as entradas alheias no PATH.
function Get-PathEntryKey {
    param([string]$Entry)
    return [Environment]::ExpandEnvironmentVariables($Entry.Trim().Trim('"')).Replace('/', '\').TrimEnd('\')
}

function Get-UserPath {
    $raw = Get-UserPathValue
    if ($raw.Length -eq 0) { return @() }
    return ($raw -split ';')
}

# Test-PathEntry reports whether a directory is already in the user PATH.
function Test-PathEntry {
    param([string]$Directory)
    $target = Get-PathEntryKey $Directory
    foreach ($entry in (Get-UserPath)) {
        if ((Get-PathEntryKey $entry) -ieq $target) {
            return $true
        }
    }
    return $false
}

# Add-UserPathEntry adds a directory to the user PATH, keeping the order and
# avoiding duplicates. It returns true when the PATH was changed.
function Add-UserPathEntry {
    param([string]$Directory)
    if (Test-PathEntry -Directory $Directory) {
        return $false
    }
    $raw = Get-UserPathValue
    $separator = ''
    if ($raw.Length -gt 0 -and -not $raw.EndsWith(';')) { $separator = ';' }
    Set-UserPathValue -Value ($raw + $separator + $Directory.TrimEnd('\'))
    return $true
}

# Remove-UserPathEntry removes a directory from the user PATH. It returns
# true when the PATH was changed.
function Remove-UserPathEntry {
    param([string]$Directory)
    $target = Get-PathEntryKey $Directory
    $entries = @(Get-UserPath)
    $kept = New-Object System.Collections.Generic.List[string]
    $changed = $false
    foreach ($entry in $entries) {
        if ((Get-PathEntryKey $entry) -ieq $target) {
            $changed = $true
            continue
        }
        [void]$kept.Add($entry)
    }
    if ($changed) {
        Set-UserPathValue -Value ($kept -join ';')
    }
    return $changed
}

# Add-SessionPathEntry adds a directory to the PATH of the current session.
function Add-SessionPathEntry {
    param([string]$Directory)
    $session = $env:Path
    if ([string]::IsNullOrWhiteSpace($session)) {
        $env:Path = $Directory
        return
    }
    $parts = $session -split ';'
    $target = Get-PathEntryKey $Directory
    foreach ($part in $parts) {
        if ((Get-PathEntryKey $part) -ieq $target) {
            return
        }
    }
    $env:Path = "$Directory;$session"
}

# Get-LlmwatchProcess returns the running llmwatch processes.
function Get-LlmwatchProcess {
    $found = @(Get-Process -Name 'llmwatch' -ErrorAction SilentlyContinue)
    if ($found.Count -eq 0) {
        return @()
    }
    return $found
}

# Get-LlmwatchTask returns the scheduled task, when it exists.
function Get-LlmwatchTask {
    if (-not (Test-Dependency 'Get-ScheduledTask')) {
        return $null
    }
    try {
        return (Get-ScheduledTask -TaskName $TaskName -TaskPath '\' -ErrorAction SilentlyContinue)
    } catch {
        return $null
    }
}

# Stop-Llmwatch stops the running processes and the scheduled task.
function Stop-Llmwatch {
    $task = Get-LlmwatchTask
    if ($null -ne $task) {
        try {
            if (Test-Dependency 'Stop-ScheduledTask') {
                Stop-ScheduledTask -TaskName $TaskName -TaskPath '\' -ErrorAction SilentlyContinue
            }
        } catch {
            Write-Warn "Nao foi possivel parar a tarefa agendada: $($_.Exception.Message)"
        }
    }
    $procs = @(Get-LlmwatchProcess)
    foreach ($proc in $procs) {
        try {
            Stop-Process -Id $proc.Id -Force -ErrorAction Stop
        } catch {
            Write-Warn "Nao foi possivel encerrar o processo $($proc.Id): $($_.Exception.Message)"
        }
    }
    if ($procs.Count -gt 0) {
        Write-Info "Processo(s) llmwatch encerrado(s)."
    }
}

# --- Download ---------------------------------------------------------------

# Get-ReleaseUrls returns the URLs of the asset and of checksums.txt.
function Get-ReleaseUrls {
    param(
        [string]$Repo,
        [string]$Version,
        [string]$AssetName
    )
    if ($Version -eq 'latest') {
        $base = "https://github.com/$Repo/releases/latest/download"
    } else {
        $base = "https://github.com/$Repo/releases/download/$Version"
    }
    return @{
        Asset  = "$base/$AssetName"
        Checks = "$base/checksums.txt"
    }
}

# Get-ReleaseWithGh downloads the assets using the GitHub CLI, which works
# with private repositories.
function Get-ReleaseWithGh {
    param(
        [string]$Repo,
        [string]$Version,
        [string]$AssetName,
        [string]$TempDir
    )
    if (-not (Test-Dependency 'gh')) {
        return $false
    }
    Write-Info "Baixando com o GitHub CLI (gh)..."
    $ghArguments = @('release', 'download')
    if ($Version -ne 'latest') {
        $ghArguments += $Version
    }
    $ghArguments += @('-R', $Repo, '-p', $AssetName, '-p', 'checksums.txt', '-D', $TempDir)
    $download = Invoke-NativeCommand -Command 'gh' -Arguments $ghArguments
    $output = $download.Output
    if ($download.ExitCode -ne 0) {
        $detail = ''
        if (-not [string]::IsNullOrWhiteSpace($output)) {
            $detail = " Detalhe: $output"
        }
        throw @"
O GitHub CLI nao conseguiu baixar a release.
$detail
Confirme que 'gh auth login' foi executado e que voce tem acesso a '$Repo'.
"@
    }
    return $true
}

# Get-Release downloads the asset and the checksums file into the temporary
# directory.
function Get-Release {
    param(
        [string]$Repo,
        [string]$Version,
        [string]$AssetName,
        [string]$TempDir
    )
    $urls = Get-ReleaseUrls -Repo $Repo -Version $Version -AssetName $AssetName
    $assetPath = Join-Path $TempDir $AssetName
    $checksPath = Join-Path $TempDir 'checksums.txt'

    $previousProgress = $ProgressPreference
    $previousProtocol = [Net.ServicePointManager]::SecurityProtocol
    try {
        $ProgressPreference = 'SilentlyContinue'
        Write-Info "Baixando $AssetName..."
        # Garante o TLS 1.2, necessario para o GitHub em Windows antigos.
        [Net.ServicePointManager]::SecurityProtocol = `
            [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
        Invoke-WebRequest -Uri $urls.Asset -OutFile $assetPath -UseBasicParsing
        Invoke-WebRequest -Uri $urls.Checks -OutFile $checksPath -UseBasicParsing
        return @{ Asset = $assetPath; Checks = $checksPath }
    } catch {
        Write-Warn "Download direto falhou: $($_.Exception.Message)"
    } finally {
        $ProgressPreference = $previousProgress
        [Net.ServicePointManager]::SecurityProtocol = $previousProtocol
    }

    # The repository may be private: fall back to the GitHub CLI.
    if (-not (Test-Dependency 'gh')) {
        throw @"
Nao foi possivel baixar o llmwatch.
Se o repositorio '$Repo' for privado, instale o GitHub CLI (https://cli.github.com),
autentique com 'gh auth login' e execute o instalador novamente.
"@
    }

    # gh recusa sobrescrever arquivos deixados por um download incompleto.
    foreach ($partialPath in @($assetPath, $checksPath)) {
        if (Test-Path -LiteralPath $partialPath) {
            Remove-Item -LiteralPath $partialPath -Force
        }
    }
    [void](Get-ReleaseWithGh -Repo $Repo -Version $Version -AssetName $AssetName -TempDir $TempDir)

    if (-not (Test-Path -LiteralPath $assetPath)) {
        throw "O GitHub CLI nao baixou o arquivo '$AssetName'."
    }
    if (-not (Test-Path -LiteralPath $checksPath)) {
        throw "O GitHub CLI nao baixou o arquivo 'checksums.txt'."
    }
    return @{ Asset = $assetPath; Checks = $checksPath }
}

# Test-Checksum compares the SHA256 of the file with the one published in
# checksums.txt. It returns the expected hash.
function Test-Checksum {
    param(
        [string]$FilePath,
        [string]$ChecksPath
    )
    $name = Split-Path $FilePath -Leaf
    $expected = $null
    foreach ($line in (Get-Content -LiteralPath $ChecksPath -Encoding UTF8)) {
        # Aceitar espacos iniciais, tabs, CRLF e o marcador binario '*'.
        if ($line -notmatch '^\s*([0-9a-fA-F]{64})\s+\*?(.+?)\s*$') { continue }
        $hash = $Matches[1]
        $file = $Matches[2]
        if ([System.IO.Path]::GetFileName($file) -ieq $name) {
            $expected = $hash
            break
        }
    }
    if ([string]::IsNullOrWhiteSpace($expected)) {
        throw "O arquivo '$name' nao esta listado em checksums.txt."
    }

    $actual = (Get-FileHash -LiteralPath $FilePath -Algorithm SHA256).Hash
    if ($actual -ine $expected) {
        throw @"
Checksum divergente em '$name'.
  esperado: $expected
  obtido:   $actual
A instalacao foi abortada.
"@
    }
    return $expected
}

# --- Tarefa agendada --------------------------------------------------------

# New-HiddenLauncher writes the VBScript that starts the server without a
# window and returns its path.
function New-HiddenLauncher {
    param(
        [string]$ExePath,
        [string]$VbsPath
    )
    $escaped = $ExePath.Replace('"', '""')
    $content = @(
        'Set shell = CreateObject("Wscript.Shell")'
        ('shell.Run """{0}"" serve", 0, False' -f $escaped)
    )
    # O .ps1 e ASCII; o VBS precisa preservar nomes de usuario com acentos.
    Set-Content -LiteralPath $VbsPath -Value $content -Encoding Unicode
    return $VbsPath
}

# Register-LlmwatchTask creates the scheduled task that starts the server
# at every logon of the current user.
function Register-LlmwatchTask {
    param(
        [string]$VbsPath
    )
    if (-not (Test-Dependency 'Register-ScheduledTask')) {
        throw "O modulo ScheduledTasks nao esta disponivel nesta versao do PowerShell."
    }

    $wscriptPath = Join-Path $env:SystemRoot 'System32\wscript.exe'
    $action = New-ScheduledTaskAction -Execute $wscriptPath -Argument "`"$VbsPath`""
    $trigger = New-ScheduledTaskTrigger -AtLogOn -User $TaskUser
    $settings = New-ScheduledTaskSettingsSet `
        -AllowStartIfOnBatteries `
        -DontStopIfGoingOnBatteries `
        -ExecutionTimeLimit ([TimeSpan]::Zero) `
        -RestartCount 3 `
        -RestartInterval (New-TimeSpan -Minutes 1)
    $principal = New-ScheduledTaskPrincipal -UserId $TaskUser -LogonType Interactive -RunLevel Limited

    Register-ScheduledTask -TaskName $TaskName -TaskPath '\' `
        -Action $action `
        -Trigger $trigger `
        -Settings $settings `
        -Principal $principal `
        -Force | Out-Null

    Start-ScheduledTask -TaskName $TaskName -TaskPath '\'
}

# --- Instalacao -------------------------------------------------------------

function Install-Llmwatch {
    Write-Host ""
    Write-Host "llmwatch - instalador para Windows" -ForegroundColor White
    Write-Host "---------------------------------" -ForegroundColor White

    # 1. Arquitetura
    $arch = Get-Architecture
    Write-Step "Arquitetura: $arch"

    # 2. Versao e repositorio
    $repo    = Get-Repo
    $version = Get-Version
    Write-Step "Repositorio: $repo"
    Write-Step "Versao: $version"

    $assetName = "llmwatch_windows_$arch.zip"

    # 3. Download
    $tempRoot = $env:TEMP
    if ([string]::IsNullOrWhiteSpace($tempRoot)) {
        $tempRoot = [System.IO.Path]::GetTempPath()
    }
    $tempDir = Join-Path $tempRoot ("llmwatch-install-" + [System.Guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $tempDir -Force | Out-Null
    try {
        Write-Step "Baixando os arquivos da release"
        $files = Get-Release -Repo $repo -Version $version -AssetName $assetName -TempDir $tempDir
        Write-Ok "Download concluido."

        # 4. Checksum
        Write-Step "Verificando o checksum SHA256"
        $hash = Test-Checksum -FilePath $files.Asset -ChecksPath $files.Checks
        Write-Ok "Checksum confere ($hash)."

        # 5. Binario
        Write-Step "Parando a instalacao anterior"
        Stop-Llmwatch

        Write-Step "Instalando o binario"
        $extractDir = Join-Path $tempDir 'extract'
        New-Item -ItemType Directory -Path $extractDir -Force | Out-Null
        Expand-Archive -LiteralPath $files.Asset -DestinationPath $extractDir -Force

        $exeSource = Get-ChildItem -LiteralPath $extractDir -Filter 'llmwatch.exe' -Recurse -File |
            Select-Object -First 1
        if ($null -eq $exeSource) {
            throw "O arquivo 'llmwatch.exe' nao foi encontrado dentro de '$assetName'."
        }

        $binDir = Get-BinDir
        New-Item -ItemType Directory -Path $binDir -Force | Out-Null
        $exePath = Join-Path $binDir 'llmwatch.exe'
        Copy-Item -LiteralPath $exeSource.FullName -Destination $exePath -Force
        Write-Ok "Binario instalado em $exePath"

        Write-Step "Configurando o PATH"
        if (Add-UserPathEntry -Directory $binDir) {
            Write-Ok "PATH do usuario atualizado."
        } else {
            Write-Info "O diretorio ja estava no PATH do usuario."
        }
        Add-SessionPathEntry -Directory $binDir
    } finally {
        if (Test-Path -LiteralPath $tempDir) {
            Remove-Item -LiteralPath $tempDir -Recurse -Force -ErrorAction SilentlyContinue
        }
    }

    # 6. Init
    Write-Step "Inicializando o llmwatch"
    $initOutput = ''
    try {
        $init = Invoke-NativeCommand -Command $exePath -Arguments @('init')
        $initOutput = $init.Output
    } catch {
        throw "Falha ao executar 'llmwatch init': $($_.Exception.Message)"
    }
    if (-not [string]::IsNullOrWhiteSpace($initOutput)) {
        foreach ($line in ($initOutput -split "`r?`n")) {
            Write-Info $line
        }
    }
    if ($init.ExitCode -ne 0) {
        throw "'llmwatch init' falhou com codigo $($init.ExitCode)."
    }

    # 7. Endereco
    if (-not [string]::IsNullOrWhiteSpace($env:LLMWATCH_ADDR)) {
        [Environment]::SetEnvironmentVariable('LLMWATCH_ADDR', $env:LLMWATCH_ADDR.Trim(), 'User')
        Write-Ok "LLMWATCH_ADDR=$($env:LLMWATCH_ADDR.Trim()) salvo no perfil do usuario."
    }

    # 8. Execucao em segundo plano
    $addr = Get-Address
    if ($env:LLMWATCH_NO_SERVICE -eq '1') {
        Write-Step "Inicializacao automatica desativada (LLMWATCH_NO_SERVICE=1)"
        Write-Info "Para subir o servidor manualmente: & `"$exePath`" serve"
    } else {
        Write-Step "Registrando a inicializacao automatica"
        $vbsPath = Join-Path (Get-InstallDir) 'llmwatch-hidden.vbs'
        [void](New-HiddenLauncher -ExePath $exePath -VbsPath $vbsPath)
        Write-Ok "Launcher criado em $vbsPath"

        try {
            Register-LlmwatchTask -VbsPath $vbsPath
            Write-Ok "Tarefa agendada '$TaskName' registrada e iniciada."
        } catch {
            Write-Warn "Nao foi possivel registrar a tarefa agendada: $($_.Exception.Message)"
            Write-Info "Voce ainda pode iniciar o servidor manualmente: & `"$exePath`" serve"
        }
    }

    # 9. Resumo
    Write-Host ""
    Write-Host "Instalacao concluida" -ForegroundColor Green
    Write-Host "---------------------" -ForegroundColor Green

    $versionOutput = ''
    try {
        $versionResult = Invoke-NativeCommand -Command $exePath -Arguments @('version')
        if ($versionResult.ExitCode -eq 0) { $versionOutput = $versionResult.Output }
    } catch {
        $versionOutput = ''
    }
    if (-not [string]::IsNullOrWhiteSpace($versionOutput)) {
        Write-Host "  Versao:      $versionOutput"
    }
    Write-Host "  URL:         http://$addr"
    Write-Host "  Binario:     $exePath"

    if ($initOutput -match 'Senha inicial:\s*(\S+)') {
        Write-Host ""
        Write-Host "  Senha inicial: $($Matches[1])" -ForegroundColor Yellow
        Write-Host "  Troque a senha no primeiro acesso ao painel." -ForegroundColor Yellow
    } elseif ($initOutput -match 'j[a\u00e1] inicializado') {
        Write-Host ""
        Write-Host "  O llmwatch ja estava inicializado (senha existente mantida)." -ForegroundColor Gray
    }

    Write-Host ""
    Write-Host "  Status line do Claude Code" -ForegroundColor Cyan
    Write-Host "  Adicione ao arquivo $env:USERPROFILE\.claude\settings.json:"
    Write-Host '    {"statusLine":{"type":"command","command":"llmwatch statusline"}}'
    Write-Host ""
    Write-Host "  Abra um novo terminal para que o PATH atualizado tenha efeito." -ForegroundColor Yellow
    Write-Host ""
}

# --- Desinstalacao ----------------------------------------------------------

function Uninstall-Llmwatch {
    Write-Host ""
    Write-Host "llmwatch - desinstalador para Windows" -ForegroundColor White
    Write-Host "-----------------------------------" -ForegroundColor White

    # 1. Tarefa agendada
    Write-Step "Removendo a tarefa agendada"
    $task = Get-LlmwatchTask
    if ($null -ne $task) {
        try {
            if (Test-Dependency 'Unregister-ScheduledTask') {
                Unregister-ScheduledTask -TaskName $TaskName -TaskPath '\' -Confirm:$false
            } else {
                throw "O comando Unregister-ScheduledTask nao esta disponivel."
            }
            Write-Ok "Tarefa '$TaskName' removida."
        } catch {
            Write-Warn "Nao foi possivel remover a tarefa agendada: $($_.Exception.Message)"
        }
    } else {
        Write-Info "A tarefa '$TaskName' nao existe."
    }

    # 2. Processo
    Write-Step "Encerrando o processo"
    Stop-Llmwatch

    # 3. PATH do usuario
    Write-Step "Removendo o diretorio do PATH"
    $binDir = Get-BinDir
    if (Remove-UserPathEntry -Directory $binDir) {
        Write-Ok "PATH do usuario atualizado."
    } else {
        Write-Info "O diretorio nao estava no PATH do usuario."
    }

    # 4. Arquivos do programa
    Write-Step "Removendo os arquivos do programa"
    $installDir = Get-InstallDir
    if (Test-Path -LiteralPath $installDir) {
        Remove-Item -LiteralPath $installDir -Recurse -Force
        Write-Ok "Removido: $installDir"
    } else {
        Write-Info "O diretorio '$installDir' nao existe."
    }

    # 5. Dados
    $dataDir = Get-DataDir
    if ($Purge) {
        Write-Step "Removendo os dados"
        if (-not (Test-Path -LiteralPath $dataDir)) {
            Write-Info "O diretorio '$dataDir' nao existe."
        } elseif ($env:LLMWATCH_YES -eq '1') {
            Remove-Item -LiteralPath $dataDir -Recurse -Force
            Write-Ok "Removido: $dataDir"
        } else {
            Write-Host ""
            Write-Host "ATENCAO: esta operacao apaga o banco de dados, a senha e os" -ForegroundColor Yellow
            Write-Host "configuracoes do llmwatch em:" -ForegroundColor Yellow
            Write-Host "  $dataDir" -ForegroundColor Yellow
            $answer = Read-Host "Confirmar a remocao dos dados? (s/N)"
            if ($answer -match '^[sSyY]') {
                Remove-Item -LiteralPath $dataDir -Recurse -Force
                Write-Ok "Removido: $dataDir"
            } else {
                Write-Info "Remocao cancelada; os dados foram mantidos."
            }
        }
    } else {
        Write-Host ""
        Write-Host "  Os dados foram mantidos em $dataDir" -ForegroundColor Gray
        Write-Host "  Use -Purge para apaga-los tambem." -ForegroundColor Gray
    }

    Write-Host ""
    Write-Host "Desinstalacao concluida" -ForegroundColor Green
    Write-Host "Abra um novo terminal para que o PATH atualizado tenha efeito." -ForegroundColor Yellow
    Write-Host ""
}

# --- Execucao ---------------------------------------------------------------

try {
    if ([Environment]::OSVersion.Platform -ne [PlatformID]::Win32NT) {
        throw 'Este instalador requer Windows.'
    }
    if ($PSVersionTable.PSVersion -lt [version]'5.1') {
        throw 'Este instalador requer Windows PowerShell 5.1 ou PowerShell 7.'
    }
    if ($Uninstall) {
        Uninstall-Llmwatch
    } else {
        Install-Llmwatch
    }
} catch {
    Write-Host ""
    Write-Err $_.Exception.Message
    Write-Host ""
    Write-Host "A operacao foi cancelada." -ForegroundColor Red
    # "return" em vez de "exit": com "irm | iex" o exit fecharia a janela do PowerShell.
    return
}
} -Uninstall:($Uninstall -or $env:LLMWATCH_UNINSTALL -eq '1') `
  -Purge:($Purge -or $env:LLMWATCH_PURGE -eq '1')
