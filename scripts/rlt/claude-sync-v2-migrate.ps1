# claude-sync v2 migration for Windows (PowerShell 5.1+). Version 20261019-v1.1.
#
# Installs the RLT claude-sync v2 binary (hash-pinned GitHub release), upgrades
# this machine's v1 config and state with `claude-sync migrate`, and shows the
# plan counts. With -PlanOnly it stops there; otherwise it asks before the
# first sync. Safe to re-run. ASCII only on purpose (PowerShell 5.1 reads
# BOM-less files as ANSI).
#
#   powershell -NoProfile -ExecutionPolicy Bypass -File "$env:USERPROFILE\.claude\scripts\claude-sync-v2-migrate.ps1" -PlanOnly
#   powershell -NoProfile -ExecutionPolicy Bypass -File "$env:USERPROFILE\.claude\scripts\claude-sync-v2-migrate.ps1"
param([switch]$PlanOnly)
$ErrorActionPreference = 'Stop'
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

$Version = '2.0.0-rlt.1'
$Url = "https://github.com/RLT-edward-curt/claude-sync/releases/download/v$Version/claude-sync-windows-amd64.exe"
$Sha = 'd0612dc928daff090942cc2e4cc7541cdc7feb19fb964c3572ab6f399cc0523d'
$InstallDir = Join-Path $env:LOCALAPPDATA 'Programs\claude-sync'
$Stamp = Get-Date -Format 'yyyyMMdd-HHmmss'

function Test-V2([string]$exe) {
    if (-not (Test-Path $exe)) { return $false }
    try { return ((& $exe version 2>$null) -join ' ') -like "*$Version*" } catch { return $false }
}

# Decide where the binary goes. A real claude-sync.exe on PATH is replaced in
# place; an npm .cmd/.ps1 shim (or nothing) means install to our own folder and
# put it first on the user PATH.
$existing = Get-Command claude-sync -ErrorAction SilentlyContinue | Select-Object -First 1
$target = Join-Path $InstallDir 'claude-sync.exe'
if ($existing -and $existing.Source -like '*.exe' -and $existing.Source -notlike '*node_modules*') {
    $target = $existing.Source
}

if (Test-V2 $target) {
    Write-Output "claude-sync $Version already installed at $target"
} else {
    # Download next to the target, not into %TEMP%: TEMP is often an 8.3 short
    # path (C:\Users\EDWARD~1.CUR\...) that Move-Item in PowerShell 5.1 rejects.
    New-Item -ItemType Directory -Force -Path (Split-Path $target) | Out-Null
    $tmp = "$target.download-$Stamp"
    Write-Output 'downloading claude-sync-windows-amd64.exe ...'
    Invoke-WebRequest -UseBasicParsing -Uri $Url -OutFile $tmp
    $got = (Get-FileHash -Algorithm SHA256 -LiteralPath $tmp).Hash.ToLower()
    if ($got -ne $Sha) {
        Remove-Item -LiteralPath $tmp -Force
        throw "checksum mismatch (got $got); nothing installed"
    }
    Unblock-File -LiteralPath $tmp
    if (Test-Path -LiteralPath $target) {
        $backup = "$target.v1-backup-$Stamp"
        Copy-Item -LiteralPath $target -Destination $backup
        Write-Output "backed up old binary -> $backup"
    }
    Move-Item -LiteralPath $tmp -Destination $target -Force
    Write-Output "installed $target"
}

# Make sure our folder wins on PATH when we installed into it.
if ($target -eq (Join-Path $InstallDir 'claude-sync.exe')) {
    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    if (-not $userPath) { $userPath = '' }
    if (($userPath -split ';') -notcontains $InstallDir) {
        [Environment]::SetEnvironmentVariable('Path', "$InstallDir;$userPath", 'User')
        Write-Output "added $InstallDir to the front of your user PATH"
    }
    if (($env:Path -split ';') -notcontains $InstallDir) { $env:Path = "$InstallDir;$env:Path" }
}
$resolved = (Get-Command claude-sync -ErrorAction SilentlyContinue | Select-Object -First 1).Source
if ($resolved -ne $target) {
    Write-Output "WARNING: 'claude-sync' still resolves to $resolved. Use $target or remove the old one."
}
& $target version

if (-not (Test-Path (Join-Path $env:USERPROFILE '.claude-sync\config.yaml'))) {
    Write-Output "no config on this machine yet: run 'claude-sync setup' in your own terminal."
    exit 0
}

& $target migrate
if ($LASTEXITCODE -ne 0) { throw "claude-sync migrate failed (exit $LASTEXITCODE)" }

if ($PlanOnly) {
    Write-Output 'plan only: review the counts above, then run: claude-sync'
    exit 0
}
$answer = ''
try { $answer = Read-Host 'Run the first sync now? [y/N]' } catch { $answer = '' }
if ($answer -match '^(y|yes)$') { & $target } else { Write-Output "not synced. Run 'claude-sync' when ready." }
