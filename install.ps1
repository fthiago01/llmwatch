<#
.SYNOPSIS
    Instalador do llmwatch para Windows.

.DESCRIPTION
    Baixa o binário do llmwatch a partir das releases do GitHub, valida o
    checksum SHA256, instala em %LOCALAPPDATA%\llmwatch\bin, adiciona o
    diretório ao PATH do usuário e registra uma tarefa agendada que sobe o
    servidor em segundo plano a cada login, sem abrir janela.

.PARAMETER Uninstall
    Remove a instalação: tarefa agendada, processo, binário e o diretório
    do PATH. Mantém os dados em %APPDATA%\llmwatch.

.PARAMETER Purge
    Com -Uninstall, também apaga os dados em %APPDATA%\llmwatch. Pede
    confirmação, a menos que $env:LLMWATCH_YES='1'.

.EXAMPLE
    irm https://raw.githubusercontent.com/fthiago01/llmwatch/main/install.ps1 | iex

.EXAMPLE
    & ([scriptblock]::Create((irm https://raw.githubusercontent.com/fthiago01/llmwatch/main/install.ps1))) -Uninstall
#>
param(
    [switch]$Uninstall,
    [switch]$Purge
)

$ErrorActionPreference = 'Stop'

# --- Constantes -------------------------------------------------------------

$script:DefaultRepo    = 'fthiago01/llmwatch'
$script:TaskName      = 'llmwatch'
$script:InstallDirName = 'llmwatch'
$script:DefaultAddr   = '127.0.0.1:18123'
$script:TaskUser       = "$env:USERDOMAIN\$env:USERNAME"

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
    return $script:DefaultRepo
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
    return (Join-Path $env:LOCALAPPDATA $script:InstallDirName)
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
    return (Join-Path $env:APPDATA $script:InstallDirName)
}

# Get-Address returns the address the server listens on.
function Get-Address {
    if (-not [string]::IsNullOrWhiteSpace($env:LLMWATCH_ADDR)) {
        return $env:LLMWATCH_ADDR.Trim()
    }
    return $script:DefaultAddr
}

# Get-UserPath returns the PATH of the current user, without duplicates.
#
# The comparison ignores the case, as Windows treats the paths as case
# insensitive.
function Get-UserPath {
    $raw = [Environment]::GetEnvironmentVariable('Path', 'User')
    if ([string]::IsNullOrWhiteSpace($raw)) {
        return @()
    }
    $entries = New-Object System.Collections.Generic.List[string]
    foreach ($part in $raw -split ';') {
        $trimmed = $part.Trim()
        if ($trimmed.Length -eq 0) { continue }
        $duplicate = $false
        foreach ($existing in $entries) {
            if ($existing -ieq $trimmed) {
                $duplicate = $true
                break
            }
        }
        if (-not $duplicate) {
            [void]$entries.Add($trimmed)
        }
    }
    return $entries.ToArray()
}

# Test-PathEntry reports whether a directory is already in the user PATH.
function Test-PathEntry {
    param([string]$Directory)
    $target = $Directory.TrimEnd('\')
    foreach ($entry in (Get-UserPath)) {
        if ($entry.TrimEnd('\') -ieq $target) {
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
    $entries = @(Get-UserPath)
    $new = New-Object System.Collections.Generic.List[string]
    foreach ($entry in $entries) { [void]$new.Add($entry) }
    [void]$new.Add($Directory.TrimEnd('\'))
    [Environment]::SetEnvironmentVariable('Path', ($new -join ';'), 'User')
    return $true
}

# Remove-UserPathEntry removes a directory from the user PATH. It returns
# true when the PATH was changed.
function Remove-UserPathEntry {
    param([string]$Directory)
    $target = $Directory.TrimEnd('\')
    $entries = @(Get-UserPath)
    $kept = New-Object System.Collections.Generic.List[string]
    $changed = $false
    foreach ($entry in $entries) {
        if ($entry.TrimEnd('\') -ieq $target) {
            $changed = $true
            continue
        }
        [void]$kept.Add($entry)
    }
    if ($changed) {
        [Environment]::SetEnvironmentVariable('Path', ($kept -join ';'), 'User')
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
    $target = $Directory.TrimEnd('\')
    foreach ($part in $parts) {
        if ($part.Trim().TrimEnd('\') -ieq $target) {
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
        return (Get-ScheduledTask -TaskName $script:TaskName -ErrorAction SilentlyContinue)
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
                Stop-ScheduledTask -TaskName $script:TaskName -ErrorAction SilentlyContinue
            }
        } catch {
            Write-Warn "Nao foi possivel parar a tarefa agendada: $($_.Exception.Message)"
        }
    }
    $procs = Get-LlmwatchProcess
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
    $args = @('release', 'download')
    if ($Version -ne 'latest') {
        $args += $Version
    }
    $args += @('-R', $Repo, '-p', $AssetName, '-p', 'checksums.txt', '-D', $TempDir)
    $output = (& gh @args 2>&1 | Out-String).Trim()
    if ($LASTEXITCODE -ne 0) {
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

    try {
        Write-Info "Baixando $AssetName..."
        # Garante o TLS 1.2, necessario para o GitHub em Windows antigos.
        [Net.ServicePointManager]::SecurityProtocol = `
            [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
        Invoke-WebRequest -Uri $urls.Asset -OutFile $assetPath -UseBasicParsing
        Invoke-WebRequest -Uri $urls.Checks -OutFile $checksPath -UseBasicParsing
        return @{ Asset = $assetPath; Checks = $checksPath }
    } catch {
        Write-Warn "Download direto falhou: $($_.Exception.Message)"
    }

    # The repository may be private: fall back to the GitHub CLI.
    if (-not (Test-Dependency 'gh')) {
        throw @"
Nao foi possivel baixar o llmwatch.
Se o repositorio '$Repo' for privado, instale o GitHub CLI (https://cli.github.com),
autentique com 'gh auth login' e execute o instalador novamente.
"@
    }

    [void](Get-ReleaseWithGh -Repo $Repo -Version $Version -AssetName $AssetName -TempDir $TempDir)

    if (-not (Test-Path $assetPath)) {
        throw "O GitHub CLI nao baixou o arquivo '$AssetName'."
    }
    if (-not (Test-Path $checksPath)) {
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
    foreach ($line in (Get-Content $ChecksPath)) {
        if ([string]::IsNullOrWhiteSpace($line)) { continue }
        $parts = $line -split '\s+', 2
        if ($parts.Count -lt 2) { continue }
        $hash = $parts[0].Trim()
        $file = $parts[1].Trim()
        # Remove the prefix '*' used by binary mode in sha256sum.
        $file = $file.TrimStart('*')
        if ([System.IO.Path]::GetFileName($file) -ieq $name) {
            $expected = $hash
            break
        }
    }
    if ([string]::IsNullOrWhiteSpace($expected)) {
        throw "O arquivo '$name' nao esta listado em checksums.txt."
    }

    $actual = (Get-FileHash -Path $FilePath -Algorithm SHA256).Hash
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
    $content = @"
Set shell = CreateObject("Wscript.Shell")
shell.Run """$escaped"" serve", 0, False
"@
    Set-Content -Path $VbsPath -Value $content -Encoding ASCII
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

    $action = New-ScheduledTaskAction -Execute 'wscript.exe' -Argument "`"$VbsPath`""
    $trigger = New-ScheduledTaskTrigger -AtLogOn -User $script:TaskUser
    $settings = New-ScheduledTaskSettingsSet `
        -AllowStartIfOnBatteries `
        -DontStopIfGoingOnBatteries `
        -ExecutionTimeLimit ([TimeSpan]::Zero) `
        -RestartCount 3 `
        -RestartInterval (New-TimeSpan -Minutes 1)
    $principal = New-ScheduledTaskPrincipal -UserId $script:TaskUser -LogonType Interactive -RunLevel Limited

    Register-ScheduledTask -TaskName $script:TaskName `
        -Action $action `
        -Trigger $trigger `
        -Settings $settings `
        -Principal $principal `
        -Force | Out-Null

    Start-ScheduledTask -TaskName $script:TaskName
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
        Expand-Archive -Path $files.Asset -DestinationPath $extractDir -Force

        $exeSource = Get-ChildItem -Path $extractDir -Filter 'llmwatch.exe' -Recurse -File |
            Select-Object -First 1
        if ($null -eq $exeSource) {
            throw "O arquivo 'llmwatch.exe' nao foi encontrado dentro de '$assetName'."
        }

        $binDir = Get-BinDir
        New-Item -ItemType Directory -Path $binDir -Force | Out-Null
        $exePath = Join-Path $binDir 'llmwatch.exe'
        Copy-Item -Path $exeSource.FullName -Destination $exePath -Force
        Write-Ok "Binario instalado em $exePath"

        Write-Step "Configurando o PATH"
        if (Add-UserPathEntry -Directory $binDir) {
            Write-Ok "PATH do usuario atualizado."
        } else {
            Write-Info "O diretorio ja estava no PATH do usuario."
        }
        Add-SessionPathEntry -Directory $binDir
    } finally {
        if (Test-Path $tempDir) {
            Remove-Item -Path $tempDir -Recurse -Force -ErrorAction SilentlyContinue
        }
    }

    # 6. Init
    Write-Step "Inicializando o llmwatch"
    $initOutput = ''
    try {
        $initOutput = (& $exePath init 2>&1 | Out-String).Trim()
        if ($LASTEXITCODE -ne 0) {
            throw "'llmwatch init' falhou com codigo $LASTEXITCODE."
        }
    } catch {
        throw "Falha ao executar 'llmwatch init': $($_.Exception.Message)"
    }
    if (-not [string]::IsNullOrWhiteSpace($initOutput)) {
        foreach ($line in ($initOutput -split "`r?`n")) {
            Write-Info $line
        }
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
        Write-Info "Para subir o servidor manualmente: `"$exePath`" serve"
    } else {
        Write-Step "Registrando a inicializacao automatica"
        $vbsPath = Join-Path (Get-InstallDir) 'llmwatch-hidden.vbs'
        [void](New-HiddenLauncher -ExePath $exePath -VbsPath $vbsPath)
        Write-Ok "Launcher criado em $vbsPath"

        try {
            Register-LlmwatchTask -VbsPath $vbsPath
            Write-Ok "Tarefa agendada '$script:TaskName' registrada e iniciada."
        } catch {
            Write-Warn "Nao foi possivel registrar a tarefa agendada: $($_.Exception.Message)"
            Write-Info "Voce ainda pode iniciar o servidor manualmente: `"$exePath`" serve"
        }
    }

    # 9. Resumo
    Write-Host ""
    Write-Host "Instalacao concluida" -ForegroundColor Green
    Write-Host "---------------------" -ForegroundColor Green

    $versionOutput = ''
    try {
        $versionOutput = (& $exePath version 2>&1 | Out-String).Trim()
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
    } elseif ($initOutput -match 'ja inicializado') {
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
                Unregister-ScheduledTask -TaskName $script:TaskName -Confirm:$false
            }
            Write-Ok "Tarefa '$script:TaskName' removida."
        } catch {
            Write-Warn "Nao foi possivel remover a tarefa agendada: $($_.Exception.Message)"
        }
    } else {
        Write-Info "A tarefa '$script:TaskName' nao existe."
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
    if (Test-Path $installDir) {
        Remove-Item -Path $installDir -Recurse -Force -ErrorAction SilentlyContinue
        Write-Ok "Removido: $installDir"
    } else {
        Write-Info "O diretorio '$installDir' nao existe."
    }

    # 5. Dados
    $dataDir = Get-DataDir
    if ($Purge) {
        Write-Step "Removendo os dados"
        if (-not (Test-Path $dataDir)) {
            Write-Info "O diretorio '$dataDir' nao existe."
        } elseif ($env:LLMWATCH_YES -eq '1') {
            Remove-Item -Path $dataDir -Recurse -Force -ErrorAction SilentlyContinue
            Write-Ok "Removido: $dataDir"
        } else {
            Write-Host ""
            Write-Host "ATENCAO: esta operacao apaga o banco de dados, a senha e os" -ForegroundColor Yellow
            Write-Host "configuracoes do llmwatch em:" -ForegroundColor Yellow
            Write-Host "  $dataDir" -ForegroundColor Yellow
            $answer = Read-Host "Confirmar a remocao dos dados? (s/N)"
            if ($answer -match '^[sSyY]') {
                Remove-Item -Path $dataDir -Recurse -Force -ErrorAction SilentlyContinue
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
