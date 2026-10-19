#!/bin/bash
# claude-sync v2 migration for macOS. Version 20261019-v2.0.
#
# Installs the RLT claude-sync v2 binary (hash-pinned GitHub release), upgrades
# this machine's v1 config and state with `claude-sync migrate`, and shows the
# plan counts. With --plan-only it stops there; otherwise it asks before the
# first sync. Safe to re-run. Rollback: restore the *.v1-backup-* binary next
# to the installed one and ~/.claude-sync/backups/config.yaml.v1-*.
#
#   bash ~/.claude/scripts/claude-sync-v2-migrate.sh --plan-only
#   bash ~/.claude/scripts/claude-sync-v2-migrate.sh
set -euo pipefail

VERSION="2.0.0-rlt.2"
BASE="https://github.com/RLT-edward-curt/claude-sync/releases/download/v${VERSION}"
SHA_ARM64="75aa128a23c85ad26cb17786fcc3e22cd751a9298428ba5f8e20685e9f161d44"
SHA_AMD64="e8931e4d23026e58ba893b1bbc54a19353a1db281c40fff4a8408dc758456f82"

PLAN_ONLY=0
[ "${1:-}" = "--plan-only" ] && PLAN_ONLY=1

case "$(uname -m)" in
  arm64) ASSET="claude-sync-darwin-arm64"; SHA="$SHA_ARM64" ;;
  x86_64) ASSET="claude-sync-darwin-amd64"; SHA="$SHA_AMD64" ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

# No other claude-sync may be running; clear a lock left by a crashed run.
if pgrep -x claude-sync >/dev/null 2>&1; then
  echo "claude-sync is already running (pgrep -x claude-sync); wait for it to finish" >&2
  exit 1
fi
if [ -f "$HOME/.claude-sync/sync.lock" ]; then
  rm -f "$HOME/.claude-sync/sync.lock"
  echo "removed a lock file left by an earlier run that did not finish"
fi

# Where to install: replace the claude-sync currently on PATH, else Homebrew bin.
TARGET="$(command -v claude-sync || true)"
if [ -z "$TARGET" ]; then
  if [ -d /opt/homebrew/bin ]; then TARGET=/opt/homebrew/bin/claude-sync; else TARGET="$HOME/.local/bin/claude-sync"; fi
  mkdir -p "$(dirname "$TARGET")"
fi

if [ -x "$TARGET" ] && "$TARGET" --version 2>/dev/null | grep -q "$VERSION"; then
  echo "claude-sync $VERSION already installed at $TARGET"
else
  WORK="$(mktemp -d)"
  trap 'rm -rf "$WORK"' EXIT
  echo "downloading $ASSET ..."
  curl -fsSL -o "$WORK/claude-sync" "$BASE/$ASSET"
  GOT="$(shasum -a 256 "$WORK/claude-sync" | awk '{print $1}')"
  if [ "$GOT" != "$SHA" ]; then
    echo "checksum mismatch for $ASSET (got $GOT); nothing installed" >&2
    exit 1
  fi
  chmod 755 "$WORK/claude-sync"
  if [ -e "$TARGET" ] || [ -L "$TARGET" ]; then
    BACKUP="${TARGET}.backup-$(date +%Y%m%d-%H%M%S)"
    cp -L "$TARGET" "$BACKUP"
    echo "backed up old binary -> $BACKUP"
    rm -f "$TARGET" # replaces a symlink itself instead of writing through it
  fi
  cp "$WORK/claude-sync" "$TARGET"
  echo "installed $("$TARGET" version) at $TARGET"
fi

if [ ! -f "$HOME/.claude-sync/config.yaml" ]; then
  echo "no config on this machine yet: run 'claude-sync setup' in your own terminal."
  exit 0
fi

"$TARGET" migrate

if [ "$PLAN_ONLY" = 1 ]; then
  echo "plan only: review the counts above, then run: claude-sync"
  exit 0
fi
read -r -p "Run the first sync now? [y/N] " ANSWER || ANSWER=""
case "$ANSWER" in
  y|Y|yes) "$TARGET" ;;
  *) echo "not synced. Run 'claude-sync' when ready." ;;
esac
