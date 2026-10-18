#!/bin/bash
# claude-sync v2 migration for macOS. Version 20261018-v1.0.
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

VERSION="2.0.0-rlt.1"
BASE="https://github.com/RLT-edward-curt/claude-sync/releases/download/v${VERSION}"
SHA_ARM64="7015a8b68635b9ff30d4c0871cf9f2c2c9131f0ca22616104745922219258c40"
SHA_AMD64="1de700477fc52eff9bbd29c12a8f7b46d6ecb3b593498012a7cb258c61cf377e"

PLAN_ONLY=0
[ "${1:-}" = "--plan-only" ] && PLAN_ONLY=1

case "$(uname -m)" in
  arm64) ASSET="claude-sync-darwin-arm64"; SHA="$SHA_ARM64" ;;
  x86_64) ASSET="claude-sync-darwin-amd64"; SHA="$SHA_AMD64" ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

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
    BACKUP="${TARGET}.v1-backup-$(date +%Y%m%d-%H%M%S)"
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
