# claude-sync v2 migration for Windows (PowerShell 5.1+). Version 20261019-v2.0.
#
# Installs the RLT claude-sync v2 binary (hash-pinned GitHub release), upgrades
# this machine's v1 config and state with `claude-sync migrate`, and shows the
# plan counts. With -PlanOnly it stops there; otherwise it asks before the
# first sync. Safe to re-run, including after a crash. ASCII only on purpose
# (PowerShell 5.1 reads BOM-less files as ANSI).
#
#   powershell -NoProfile -ExecutionPolicy Bypass -File "$env:USERPROFILE\.claude\scripts\claude-sync-v2-migrate.ps1" -PlanOnly
#   powershell -NoProfile -ExecutionPolicy Bypass -File "$env:USERPROFILE\.claude\scripts\claude-sync-v2-migrate.ps1"
param([switch]$PlanOnly)
$ErrorActionPreference = 'Stop'
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

$Version = '2.0.0-rlt.2'
$Url = "https://github.com/RLT-edward-curt/claude-sync/releases/download/v$Version/claude-sync-windows-amd64.exe"
$Sha = '8a6c89c1678a9d128653b14ed67cd62e2c1d23af40ccdd23cd2db7c02b23ea0e'
$InstallDir = Join-Path $env:LOCALAPPDATA 'Programs\claude-sync'
$ConfigDir = Join-Path $env:USERPROFILE '.claude-sync'
$CrashLog = Join-Path $ConfigDir 'logs\crash.log'
$Stamp = Get-Date -Format 'yyyyMMdd-HHmmss'

# Run a native command without PowerShell 5.1 turning its stderr (progress
# lines) into terminating errors. Returns the exit code.
function Invoke-Native([string]$exe, [string[]]$argv) {
    $old = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try { & $exe @argv 2>&1 | ForEach-Object { Write-Host "$_" } } finally { $ErrorActionPreference = $old }
    return $LASTEXITCODE
}

function Test-V2([string]$exe) {
    if (-not (Test-Path -LiteralPath $exe)) { return $false }
    $old = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try { return ((& $exe version 2>$null) -join ' ') -like "*$Version*" } catch { return $false } finally { $ErrorActionPreference = $old }
}

function Show-Crash {
    if (Test-Path -LiteralPath $CrashLog) {
        Write-Output "---- last lines of $CrashLog (send these to Claude) ----"
        Get-Content -LiteralPath $CrashLog -Tail 60
    }
}

# 1. No other claude-sync may be running; clear a lock left by a crashed run.
$running = Get-Process -Name claude-sync -ErrorAction SilentlyContinue
if ($running) {
    throw "claude-sync is already running (PID $($running.Id -join ', ')). Wait for it or stop it with: Stop-Process -Id $($running[0].Id)"
}
$lock = Join-Path $ConfigDir 'sync.lock'
if (Test-Path -LiteralPath $lock) {
    Remove-Item -LiteralPath $lock -Force
    Write-Output 'removed a lock file left by an earlier run that did not finish'
}

# 2. Decide where the binary goes. A real claude-sync.exe on PATH is replaced
# in place; an npm .cmd/.ps1 shim (or nothing) means our own folder, first on
# the user PATH.
$existing = Get-Command claude-sync -ErrorAction SilentlyContinue | Select-Object -First 1
$target = Join-Path $InstallDir 'claude-sync.exe'
if ($existing -and $existing.Source -like '*.exe' -and $existing.Source -notlike '*node_modules*') {
    $target = $existing.Source
}

# 3. Install (download beside the target: %TEMP% can be an 8.3 short path
# that Move-Item rejects).
if (Test-V2 $target) {
    Write-Output "claude-sync $Version already installed at $target"
} else {
    New-Item -ItemType Directory -Force -Path (Split-Path $target) | Out-Null
    $tmp = "$target.download-$Stamp"
    Write-Output "downloading claude-sync $Version ..."
    Invoke-WebRequest -UseBasicParsing -Uri $Url -OutFile $tmp
    $got = (Get-FileHash -Algorithm SHA256 -LiteralPath $tmp).Hash.ToLower()
    if ($got -ne $Sha) {
        Remove-Item -LiteralPath $tmp -Force
        throw "checksum mismatch (got $got); nothing installed"
    }
    Unblock-File -LiteralPath $tmp
    if (Test-Path -LiteralPath $target) {
        $backup = "$target.backup-$Stamp"
        Copy-Item -LiteralPath $target -Destination $backup
        Write-Output "backed up previous binary -> $backup"
    }
    Move-Item -LiteralPath $tmp -Destination $target -Force
    Write-Output "installed $target"
}
Get-ChildItem -LiteralPath (Split-Path $target) -Filter '*.download-*' -ErrorAction SilentlyContinue | Remove-Item -Force -ErrorAction SilentlyContinue

if ($target -eq (Join-Path $InstallDir 'claude-sync.exe')) {
    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    if (-not $userPath) { $userPath = '' }
    if (($userPath -split ';') -notcontains $InstallDir) {
        [Environment]::SetEnvironmentVariable('Path', "$InstallDir;$userPath", 'User')
        Write-Output "added $InstallDir to the front of your user PATH"
    }
}
$null = Invoke-Native $target @('version')
Write-Output "binary: $target"

if (-not (Test-Path -LiteralPath (Join-Path $ConfigDir 'config.yaml'))) {
    Write-Output "no config on this machine yet: run 'claude-sync setup' in your own terminal."
    exit 0
}

# 4. Migrate (backs up config/state, checks the key, repairs v1 damage,
# prints the read-only plan). Progress lines appear every few seconds.
$code = Invoke-Native $target @('migrate')
if ($code -ne 0) {
    Show-Crash
    throw "claude-sync migrate failed (exit $code). Re-running this script is safe."
}

if ($PlanOnly) {
    Write-Output "plan only: review the counts above. First sync: & `"$target`""
    exit 0
}
$answer = ''
try { $answer = Read-Host 'Run the first sync now? [y/N]' } catch { $answer = '' }
if ($answer -match '^(y|yes)$') {
    $code = Invoke-Native $target @()
    if ($code -ne 0) { Show-Crash; throw "sync exited with $code" }
} else {
    Write-Output "not synced. Run: & `"$target`""
}
