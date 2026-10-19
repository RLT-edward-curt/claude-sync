# claude-sync v2: Windows PC rollout (for the "my .claude config" coordinator to relay)

Status 2026-10-18: MacBook Pro is on v2 and synced (308 up, 285 down, 0 deleted; second plan clean,
R2 ETag gate passed). Release: https://github.com/RLT-edward-curt/claude-sync/releases/tag/v2.0.0-rlt.1

Why NOT `claude-sync pull` (v1) on the PC first: v1 on Windows writes Mac transcripts as invalid
JSON (raw C:\Users\... inside JSON strings). Skip it; v2 does the pull correctly and `migrate`
repairs files v1 already damaged. The script is fetched from the release tag instead and its hash
is checked (it pins the binary's SHA256 itself).

Prerequisite: whatever fragment/config work is in progress on the PC is finished and no
`claude-sync` (v1) run is in flight. config.yaml must exist at %USERPROFILE%\.claude-sync\config.yaml.

## Step 1: fetch and verify the script (run each line separately)

```powershell
New-Item -ItemType Directory -Force -Path "$env:USERPROFILE\.claude\scripts" | Out-Null
Invoke-WebRequest -UseBasicParsing -Uri "https://raw.githubusercontent.com/RLT-edward-curt/claude-sync/v2.0.0-rlt.2/scripts/rlt/claude-sync-v2-migrate.ps1" -OutFile "$env:USERPROFILE\.claude\scripts\claude-sync-v2-migrate.ps1"
(Get-FileHash -Algorithm SHA256 "$env:USERPROFILE\.claude\scripts\claude-sync-v2-migrate.ps1").Hash
```
Expected hash: `FA712DB07D76AC6F0BFF97245957FD6D7E0843F34207E65B700B90285F12DA7D`. Stop if different.

## Step 2: install + migrate + read-only plan (no transfers)

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File "$env:USERPROFILE\.claude\scripts\claude-sync-v2-migrate.ps1" -PlanOnly
cmd /c where claude-sync
```
The script prints `installed <path>` (default `%LOCALAPPDATA%\Programs\claude-sync\claude-sync.exe`,
or the existing `claude-sync.exe` location if one was a real exe on PATH) and the plan counts.
`where` shows what NEW shells will run; the already-running Claude session keeps its old PATH,
so use the full path below. RELAY THE PLAN COUNTS TO ED AND WAIT FOR HIS YES.

What to expect: many downloads (Mac/Studio transcripts and config), uploads of PC-only sessions,
0 or few remote deletions (more than 20 are held automatically), a large "remote, outside the
rules" and "stale baselines" count (the old plugins/ uploads: forgotten, never deleted), and
possibly "repaired N transcript line(s)" from the v1 damage.

## Step 3: first sync (only after Ed approves the counts)

```powershell
& "$env:LOCALAPPDATA\Programs\claude-sync\claude-sync.exe"
& "$env:LOCALAPPDATA\Programs\claude-sync\claude-sync.exe" plan
```
(If step 2 printed a different install path, use that path instead.) The second command should show
upload/download 0 apart from live session transcripts. Errors like "write (file in use?)" are a
transcript Claude Code has open; it retries next sync.

If it says `HELD N remote deletions`, do not confirm; send Ed the output of
`& "$env:LOCALAPPDATA\Programs\claude-sync\claude-sync.exe" plan -v`.

## Rollback on the PC

The old exe (if any) is backed up as `<path>.v1-backup-<timestamp>`; config/state backups are in
`%USERPROFILE%\.claude-sync\backups\`. The bucket layout is unchanged, so v1 still works.

# Mac Studio (when Ed wants it; not touched yet)

In a terminal on the Studio (one command per line):

```bash
curl -fsSL -o ~/.claude/scripts/claude-sync-v2-migrate.sh https://raw.githubusercontent.com/RLT-edward-curt/claude-sync/v2.0.0-rlt.2/scripts/rlt/claude-sync-v2-migrate.sh
shasum -a 256 ~/.claude/scripts/claude-sync-v2-migrate.sh
bash ~/.claude/scripts/claude-sync-v2-migrate.sh --plan-only
claude-sync
claude-sync plan
```
Expected shasum: `fdc6494798e5926d78dd8296c1016f23683f350137d1e168240d517741a6a20d`. Stop if different,
and review the plan counts before the bare `claude-sync`.
