// Package rules holds the one built-in definition of what claude-sync syncs.
//
// There is deliberately no per-machine override: every machine running the
// same binary version applies exactly these rules, so machines cannot drift.
// Changing a rule means shipping a new binary (bump Version).
package rules

import (
	"path"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// Version identifies this rule set. Bump it whenever Paths, Excludes or the
// size limit change; `claude-sync version` prints it.
const Version = 1

// Paths are the top-level entries under ~/.claude that sync. A directory
// entry syncs everything below it, minus Excludes.
var Paths = []string{
	"CLAUDE.md",
	"claude-cowork.md",
	"README.md",
	"settings.json",
	"settings.local.json",
	"statusline-command.sh",
	"agents",
	"commands",
	"skills",
	"hooks",
	"rules",
	"memory",
	"scripts",
	"templates",
	"projects",
	"plans",
	"tasks",
	"history.jsonl",
}

// Excludes are doublestar globs matched against forward-slash relative paths.
var Excludes = []string{
	"**/node_modules/**",
	"**/.venv/**",
	"**/*.o",
	"**/*.a",
	"**/third_party/**",
	"**/.git/**",
	"**/.DS_Store",
	"skills/synced/**",
	// Conflict copies stay on the machine that made them.
	"**/*.conflict.*",
	"**/*.conflict-*",
	// Our own atomic-write temp files.
	"**/.claude-sync-tmp-*",
}

// MaxUploadBytes is the largest encrypted object v2 will upload. It sits below
// the 100 MB download cap that v1 binaries enforce, so v1 machines never choke.
var MaxUploadBytes = 95 * 1024 * 1024

// MaxDownloadBytes is the largest remote object v2 will download.
var MaxDownloadBytes = 100 * 1024 * 1024

// InScope reports whether relPath (forward slashes, relative to ~/.claude) is
// covered by the rules: under one of Paths and not excluded.
func InScope(relPath string) bool {
	relPath = strings.TrimPrefix(path.Clean("/"+relPath), "/")
	under := false
	for _, p := range Paths {
		if relPath == p || strings.HasPrefix(relPath, p+"/") {
			under = true
			break
		}
	}
	return under && !Excluded(relPath)
}

// Excluded reports whether relPath matches an exclude glob. Directory paths
// are matched too, so a walk can skip a whole excluded tree.
func Excluded(relPath string) bool {
	for _, pat := range Excludes {
		if ok, _ := doublestar.Match(pat, relPath); ok {
			return true
		}
		// "**/x/**" should also match the directory "x" itself (and a
		// top-level "x"), so walks can prune it without descending.
		if strings.HasSuffix(pat, "/**") {
			if ok, _ := doublestar.Match(strings.TrimSuffix(pat, "/**"), relPath); ok {
				return true
			}
		}
	}
	return false
}

// ConflictClass decides how a both-sides-changed file is resolved.
type ConflictClass int

const (
	// KeepBoth keeps the local file, writes the remote copy beside it, and
	// freezes the path until the user deletes the copy.
	KeepBoth ConflictClass = iota
	// History merges local and remote lines, then rebuilds from projects/.
	History
	// Transcript takes the longer copy when one is a prefix of the other,
	// otherwise keeps local.
	Transcript
	// KeepLocal keeps the local copy and uploads it (regenerable data).
	KeepLocal
)

// ClassOf returns the conflict class for relPath.
func ClassOf(relPath string) ConflictClass {
	switch {
	case relPath == "history.jsonl":
		return History
	case strings.HasPrefix(relPath, "projects/") && strings.Contains(relPath, "/tool-results/"):
		return KeepLocal
	case strings.HasPrefix(relPath, "projects/") && strings.HasSuffix(relPath, ".jsonl"):
		return Transcript
	case strings.HasSuffix(relPath, ".cache"),
		strings.HasPrefix(relPath, "tasks/") && strings.HasSuffix(relPath, ".lock"):
		return KeepLocal
	}
	return KeepBoth
}

// Executable reports whether a newly downloaded file should get the exec bit
// on Unix: hooks, scripts and shell scripts.
func Executable(relPath string) bool {
	return strings.HasPrefix(relPath, "hooks/") ||
		strings.HasPrefix(relPath, "scripts/") ||
		strings.HasSuffix(relPath, ".sh") ||
		relPath == "statusline-command.sh"
}
