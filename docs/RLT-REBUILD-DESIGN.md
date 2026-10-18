# claude-sync v2 (RLT fork): design for approval

Status: DRAFT 20261018-v0.1, awaiting Ed's approval. Nothing below is built yet.
Branch: `rlt/v2-one-command` on `RLT-edward-curt/claude-sync`.

## 1. Commands (the whole surface)

| Command | What it does |
|---|---|
| `claude-sync` (same as `claude-sync sync`) | Lists the bucket, scans `~/.claude`, reconciles both ways, auto-resolves the safe conflicts, rebuilds `history.jsonl` if anything under `projects/` or `history.jsonl` changed, prints a 3 to 6 line summary. |
| `claude-sync plan` | Same reconcile, read-only: prints the counts (uploads, downloads, remote deletions, conflicts, too-large skips) and nothing else happens. This is what you approve before any first live run. |
| `claude-sync setup` | Prompts (hidden input, in your own terminal) for endpoint/account, bucket, access key, secret, passphrase. Optional `--op` reads them from 1Password refs instead. Proves the passphrase against the bucket, writes `~/.claude-sync/config.yaml` + key, then runs `plan` and asks before the first sync. |
| `claude-sync conflicts` | Lists unresolved keep-both conflicts and where each copy is. |
| `claude-sync rules` | Prints the built-in sync rules (what syncs, what is excluded, size limit). |
| `claude-sync migrate` | Used once by the migration script: converts this machine's v1 config and state (see section 7). Idempotent. |
| `claude-sync prune` | The ONLY way to delete remote objects that are out of scope (e.g. the old `plugins/` objects). Shows counts, requires typing the count to confirm, moves them to trash first. |
| `claude-sync mcp ...`, `rebuild-history`, `version` | Kept as is. |

Removed: `update` (prints "installs come from the migration script / GitHub release"), `reset` in all forms (the self-healing state makes `--local` unnecessary; `--remote` is gone), `init` (replaced by `setup`), `paths add/remove/exclude` (rules are built in), `auto`. `push` and `pull` remain as aliases that print one line ("now just `claude-sync`") and run `sync`.

## 2. Rules: built into the binary, nothing per machine

The rules are a Go table in `internal/rules`. Every machine on the same binary version has identical rules; `claude-sync version` prints a rules version. Any `sync_paths`/`exclude`/`scope` in `config.yaml` is ignored (one-line notice) and removed by `migrate`. Changing rules means a new release, which is the point: no fragment, no apply script, no drift.

Synced: `CLAUDE.md`, `claude-cowork.md`, `README.md`, `settings.json`, `settings.local.json`, `statusline-command.sh`, `agents/`, `commands/`, `skills/`, `hooks/`, `rules/`, `memory/`, `scripts/`, `templates/`, `projects/`, `plans/`, `tasks/`, `history.jsonl`.

Not synced: `plugins/`, `mcp-servers/`, `backups/`, everything else at the top level (including `claude-sync-shared-config.yaml`, which is retired).

Excluded anywhere: `**/node_modules/**`, `**/.venv/**`, `**/*.o`, `**/*.a`, `**/third_party/**`, `**/.DS_Store`, `**/*.conflict.*` and `**/*.conflict-*` (conflict copies stay on the machine that made them; today they get uploaded and spread).

Proposed, need your yes/no:
- `**/.git/**`: exclude. Git internals (index, packs, lock files) churn on every git command, conflict constantly, and are rebuildable by cloning. Recommend exclude.
- `skills/synced/**`: exclude. claude.ai manages and re-downloads these on each machine; syncing them makes two writers. Recommend exclude.
- `settings.local.json`: you listed it; note it is per-machine by Claude Code convention (local permission grants). Keeping it per your list, flagging only.

Size rule: no hand-listed paths. Any file whose compressed + encrypted object would exceed 95 MB is skipped on upload with a warning naming it; any remote object over 100 MB (by listed size) is skipped on download with a warning. 95 MB keeps a margin under the 100 MB cap that v1 binaries enforce on download, so v1 machines never choke during rollout.

## 3. How a sync decides (and why state can no longer lie)

Every sync lists the whole bucket first (one paginated LIST, a few seconds) and decides per file from three facts: the local file hash, the remote object's ETag from the listing, and the state entry from the last sync (local hash + remote ETag at that time). No clock comparisons.

| Local | Remote | Decision |
|---|---|---|
| changed | unchanged | upload |
| unchanged | changed | download |
| changed | changed | conflict policy (section 4) |
| present | missing, no tombstone | upload (this is the self-heal: state saying "uploaded" never suppresses an upload when the LIST says the object is not there) |
| present, unchanged | missing, tombstoned | leave the local file alone, report "deleted on another machine, still here" |
| missing, was synced | present | remote deletion candidate (section 5) |
| missing, never synced | present | download |
| present, no state entry | present | download to temp and compare hash; equal means record it, different means conflict policy (never mtime-wins) |

`state.json` stays in `~/.claude-sync/` but is a cache: it gains `remote_etag` and `schema: 2`. If it is missing or corrupt, the next sync rebuilds it from the LIST plus hashing (slow once, never wrong).

## 4. Conflict policy (both sides changed)

| Path | Policy |
|---|---|
| `history.jsonl` | Union of local and remote lines, dedupe, then rebuild from `projects/`, upload. Nothing lost. |
| `projects/**/*.jsonl` transcripts | If one is a prefix of the other (the normal append-only case), take the longer. Otherwise keep local and upload it; the summary names it. |
| Regenerable caches (`projects/**/tool-results/**`, `**/*.cache`, `tasks/**/*.lock`) | Keep local. |
| Everything else (`CLAUDE.md`, `settings*.json`, `skills/`, `agents/`, `hooks/`, `memory/`, ...) | Keep both: local stays, remote is written next to it as `<file>.conflict-<otherhost>-<yyyymmdd-hhmm>`, the path is frozen (no upload, no download) until you delete the conflict copy. After that the next sync uploads your merged local file. The summary and `claude-sync conflicts` list every frozen path. Never a silent overwrite. |

## 5. Deletion policy

1. Pull never deletes or overwrites a local file without the rules above. A remote deletion never removes your local copy.
2. A remote object is deleted only when: the path is in scope under the current rules, it was synced before, and `lstat` says the local file is truly gone. Out of scope (rules changed, path excluded, too large) means "forget from state," never "delete remote." This is the fix for the 3,094 and 243 surprise deletions.
3. Deletions are soft: the object is copied to `_metadata/trash/<yyyymmdd>/<key>` before delete, and a tombstone is written to `_metadata/tombstones.json.age`. Trash older than 30 days is purged by `prune`, never automatically.
4. Threshold: if a sync would delete more than 20 objects or more than 2% of tracked files, it stops before deleting anything, prints the count and the top directories, and asks you to type the number to confirm (interactive) or refuses (non-interactive, e.g. run by Claude) with the exact command to run.
5. Every summary line includes the deletion count, even when zero.

During rollout, v1 machines still delete without tombstones; a v2 machine will then see "present locally, missing remote, no tombstone" and re-upload. That is the safe direction (resurrect, not lose); it stops once every machine is on v2.

## 6. Compatibility with v1 (no re-upload needed)

Kept byte-for-byte: the age key file and passphrase derivation (Argon2id, fixed salt), gzip then age with `.age` suffix, the bucket root layout, the `${HOME}` normalization of `projects/<encoded-cwd>` keys, and `_metadata/manifest.json.age` (mtimes; v2 writes it in the same shape because v1 overwrites it wholesale on every push). All new v2 metadata (tombstones, trash) lives under `_metadata/` in separate keys, which v1 skips on pull. A v1 and a v2 machine can run side by side for the whole rollout. No re-upload; the first v2 sync on each machine downloads nothing it already has (hash match) and seeds ETags.

Verified constraint: `setup` and `migrate` decrypt an existing object (the manifest) with the key before writing or syncing anything. A typo in the passphrase produces a valid but different key that would make this machine's uploads unreadable elsewhere; v2 refuses instead.

## 7. Windows

- Pure Go, no bash or WSL for `setup`, `sync`, `migrate`. Hidden password input via `golang.org/x/term` works in PowerShell and Windows Terminal.
- Nothing asks you to redirect output to a file. If you ever do, the summary is plain ASCII so UTF-16 from PowerShell 5.1 `>` is still readable; the state and config files are written by the binary only.
- Downloads write to a temp file and rename. If Claude Code holds a transcript open and the rename fails, that one file is reported and retried next sync; the sync continues.
- Execute bits: files under `hooks/`, `scripts/`, and `*.sh` keep or get mode 0700 on macOS (today a pulled hook loses `+x`).
- Case collisions (two remote keys differing only in case) are reported, not written, on both Windows and macOS.

Bug found while reading, fixed as core (not optional): on Windows, v1 replaces the `${HOME}` token in Mac transcripts with the raw `C:\Users\edward.curt`, producing invalid JSON escapes (`\U`, `\e`) in every Mac transcript pulled to the PC, and it never normalizes the JSON-escaped Windows path on push. v2 resolves to the JSON-safe forward-slash form `C:/Users/edward.curt` and normalizes both the escaped and forward-slash Windows forms. A unit test will prove a Mac line resolved on Windows still parses. (Existing corrupted copies on the PC are rewritten on the next download of each file; I can also add a one-time repair in `migrate`. Your call.)

Cross-OS resume for the Code folder (Ed, 2026-10-18: rewrite every project path inside the Code folder on all machines; leave projects outside it as they are):
- Each OS has one built-in code root: macOS `~/Code` (both Macs), Windows `<WINDOWS CODE ROOT, to confirm>`. It is part of the built-in rules, not per-machine config.
- Remote keys for anything under the code root use the form v1 already writes for Mac: `projects/${HOME}-Code-<project...>`. So no Mac transcript is re-keyed or re-uploaded, and v1 Macs keep working. On Windows, v2 maps its local session folder for `<code root>\<project>` to and from that same key, wherever the Windows code root actually lives.
- Content: every path under the code root, in any of its forms (`/Users/edwardcurt/Code/...`, JSON-escaped `C:\\...\\Code\\...`, or `C:/.../Code/...`), is normalized on upload to `${HOME}/Code/<rest>` with forward slashes, and resolved on download to that machine's own code root, forward slashes on Windows so the JSON stays valid. That covers the `cwd` field `/resume` matches on and the tool-call file paths inside the transcript.
- Outside the code root (OneDrive Cowork and Claude-Containers folders, scratch dirs, home itself): unchanged from today. Keys keep v1's `${HOME}` handling, content is not rewritten beyond the JSON-escape fix above, and these sessions are still synced but not expected to resume on the other OS.
- Existing Windows sessions under the code root that v1 uploaded under a raw Windows key (if any) become legacy duplicates: v2 reads them, writes the canonical key, and leaves the old objects for `prune`.
- `migrate` re-resolves already-downloaded transcripts in place on each machine (this is the Windows repair from decision 3), and `plan` shows how many files that touches before anything happens.

## 8. Config and secrets

- `~/.claude-sync/config.yaml` (outside `~/.claude`, mode 0600) keeps only storage + key path. Never printed: no command echoes config, and errors are scrubbed of the access key and secret.
- `setup --op` reads `op://` refs with `op read`, capturing stdout straight into memory; values never touch argv, disk, or the terminal. I will read the `onepassword` skill before writing that code and will ask you for the item path.
- `CLAUDE_SYNC_HOME` and `CLAUDE_SYNC_CLAUDE_DIR` env overrides exist so every test runs against a fake home and fake `~/.claude`; tests never touch the real ones.

## 9. Binaries: GitHub Releases on the fork (recommended)

Recommend prebuilt assets over Go on every machine: the PC has no Go, and a hash-pinned download is more reproducible than a build.
- Cross-compile here with `CGO_ENABLED=0` for `darwin-arm64` and `windows-amd64` (and `darwin-amd64` just in case), publish as a prerelease `v2.0.0-rlt.1` with `gh release create` and a `SHA256SUMS` file.
- The migration scripts hardcode the SHA256 of each asset and refuse a mismatch. They travel inside the encrypted sync, so the pin cannot be tampered with in transit.
- The fork's `release.yml` (semantic-release + npm) only triggers on push to `main`; this branch never merges to `main`, so it cannot fire.

## 10. Migration scripts (distributed through the existing claude-sync)

Two thin wrappers in `~/.claude/scripts/`; all real logic is in the Go binary so Mac and Windows behave identically:
- `claude-sync-v2-migrate.sh` (macOS) and `claude-sync-v2-migrate.ps1` (Windows PowerShell 5.1, run as `powershell -ExecutionPolicy Bypass -File ...`).

Each one:
1. Finds the current `claude-sync` (`command -v` / `Get-Command`; handles an npm `.cmd` shim on Windows) and backs it up beside itself.
2. Downloads the release asset for this OS/arch, verifies the pinned SHA256, installs it in the same place (Windows: `%LOCALAPPDATA%\Programs\claude-sync\claude-sync.exe` and prepends that to the user PATH if the old one was a shim).
3. Runs `claude-sync migrate`: backs up `config.yaml` and `state.json` into `~/.claude-sync/backups/` (never into `~/.claude`), leaves `sync_paths`/`exclude` in place during rollout (v2 ignores them; a stray v1 run then behaves as before instead of falling back to upstream defaults and deleting remote files), proves the key against the bucket, upgrades `state.json` to schema 2 (seeding ETags from the LIST where the hash already matches).
4. Runs `claude-sync plan`, prints the counts, and asks y/N before the first `claude-sync`.

Rollback: restore the backed-up binary and `config.yaml`; v1 still reads the bucket because nothing in the layout changed.

## 11. Testing (scratch only)

- Unit tests for the decision table, conflict policies, deletion guard, size skip, Windows path JSON test, rules.
- Integration: two fake homes (simulating two machines, one with Windows-style home) against a scratch bucket. I recommend a separate R2 bucket (e.g. `claude-sync-scratch`) with an R2 API token scoped to that bucket only, stored in 1Password; a prefix in the live bucket is riskier because the code has no prefix concept and one missed key would hit live data. I need from you: the 1Password refs for that scratch token (account ID/endpoint, access key, secret). I never see the values.
- v1 interop test: run the current v1 binary (with `CLAUDE_SYNC_HOME` style isolation via a temp HOME) against the same scratch bucket alongside v2.

## 12. Rollout (each live step needs your explicit go)

1. Mac (this one): you push the migration script and new skill with the OLD tool. Then run the Mac script; it shows `plan` counts against the live bucket; you approve; first v2 sync.
2. Mac Studio: old-tool pull, run the script, approve counts, sync.
3. Windows PC: after the other session finishes its current fragment apply. Run through the PC's Remote Control session ("Claude-sync tool (CPC Restore)"), relayed by the "my .claude config" session after Ed approves; this session never messages the PC directly. I will deliver the steps as exact PowerShell commands, one per line, ASCII only, no output redirection, in this shape (final hashes and version filled in after the release exists):
   ```powershell
   claude-sync pull
   Get-Command claude-sync | Format-List Source
   powershell -NoProfile -ExecutionPolicy Bypass -File "$env:USERPROFILE\.claude\scripts\claude-sync-v2-migrate.ps1" -PlanOnly
   powershell -NoProfile -ExecutionPolicy Bypass -File "$env:USERPROFILE\.claude\scripts\claude-sync-v2-migrate.ps1"
   claude-sync version
   claude-sync
   ```
   The `-PlanOnly` run installs and migrates but stops after printing `plan` counts, so Ed approves the numbers before the real first sync.
4. After all three are on v2: `claude-sync prune` shows the out-of-scope objects still in the bucket (old `plugins/`, `mcp-servers/`, `backups/` leftovers, the shared fragment); you decide.

## Decisions I need from you

1. Approve the design (or mark changes).
2. `**/.git/**` and `skills/synced/**` excluded: yes/no.
3. Windows transcript repair in `migrate`: yes/no.
4. Cross-OS Code folder rewrite: DECIDED (rewrite under the Code folder only). Need the exact Windows code root path, and confirmation the Mac Studio uses `/Users/edwardcurt/Code`.
5. Scratch bucket: OK to use a separate bucket + scoped token? Send the 1Password refs when ready.
