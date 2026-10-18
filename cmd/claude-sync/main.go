// claude-sync v2 (RLT fork): one command to sync ~/.claude across machines.
// Design: docs/RLT-REBUILD-DESIGN.md.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/tawanorg/claude-sync/internal/config"
	"github.com/tawanorg/claude-sync/internal/crypto"
	"github.com/tawanorg/claude-sync/internal/rules"
	"github.com/tawanorg/claude-sync/internal/storage"
	"github.com/tawanorg/claude-sync/internal/sync"

	_ "github.com/tawanorg/claude-sync/internal/storage/gcs"
	_ "github.com/tawanorg/claude-sync/internal/storage/r2"
	_ "github.com/tawanorg/claude-sync/internal/storage/s3"
	_ "github.com/tawanorg/claude-sync/internal/storage/webdav"
)

var (
	version = "dev" // -ldflags "-X main.version=..."
	verbose bool
)

func main() {
	var confirmDeletes int
	root := &cobra.Command{
		Use:   "claude-sync",
		Short: "Sync ~/.claude across your machines (run with no arguments)",
		Long: "Run `claude-sync` with no arguments: it pushes local changes, pulls remote\n" +
			"changes, resolves the safe conflicts, rebuilds history.jsonl and prints a summary.",
		Version:       versionString(),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSync(confirmDeletes)
		},
	}
	root.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "list every file")
	root.Flags().IntVar(&confirmDeletes, "confirm-deletes", 0, "allow exactly this many remote deletions (printed when needed)")

	root.AddCommand(
		syncCmd("sync", "Same as running claude-sync with no arguments", ""),
		syncCmd("push", "Alias for sync (v2 always syncs both ways)", "note: `claude-sync` now pushes and pulls in one step."),
		syncCmd("pull", "Alias for sync (v2 always syncs both ways)", "note: `claude-sync` now pushes and pulls in one step."),
		planCmd(),
		setupCmd(),
		migrateCmd(),
		conflictsCmd(),
		rulesCmd(),
		pruneCmd(),
		rebuildHistoryCmd(),
		mcpCmd(),
		versionCmd(),
		removedCmd("update", "v2 is installed from the RLT fork's GitHub release by the migration script; the upstream updater would replace it with a build that lacks these fixes."),
		removedCmd("reset", "removed in v2. State is a self-healing cache and remote data is never wiped from this tool. To start fresh on one machine, delete ~/.claude-sync/state.json and run claude-sync."),
		removedCmd("init", "use `claude-sync setup`."),
	)

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", scrub(err.Error()))
		os.Exit(1)
	}
}

func versionString() string {
	return fmt.Sprintf("%s (rules v%d)", version, rules.Version)
}

// scrub removes credential values from any error text before printing.
func scrub(s string) string {
	cfg, err := config.Load()
	if err != nil {
		return s
	}
	sc := cfg.GetStorageConfig()
	for _, secret := range []string{sc.SecretAccessKey, sc.AccessKeyID, sc.WebDAVPassword} {
		if len(secret) >= 8 {
			s = strings.ReplaceAll(s, secret, "[redacted]")
		}
	}
	return s
}

var stdin = bufio.NewReader(os.Stdin)

func interactive() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

func ask(prompt string) string {
	fmt.Print(prompt)
	line, _ := stdin.ReadString('\n')
	return strings.TrimSpace(line)
}

func confirm(prompt string) bool {
	if !interactive() {
		return false
	}
	a := strings.ToLower(ask(prompt + " [y/N] "))
	return a == "y" || a == "yes"
}

// loadSyncer loads config and builds a syncer, noting ignored v1 rules.
func loadSyncer() (*config.Config, *sync.Syncer, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, errors.New("no config on this machine; run `claude-sync setup`")
	}
	// v1 sync_paths/exclude are deliberately left in config.yaml during the
	// rollout and ignored here: if a v1 binary still runs on this machine, it
	// keeps using them instead of falling back to v1 defaults (which would
	// delete remote hooks/, memory/, scripts/ ... and upload plugins/).
	s, err := sync.NewSyncer(cfg, !verbose)
	return cfg, s, err
}

// ---------------------------------------------------------------------------
// sync

func syncCmd(name, short, note string) *cobra.Command {
	var confirmDeletes int
	c := &cobra.Command{
		Use:   name,
		Short: short,
		RunE: func(cmd *cobra.Command, args []string) error {
			if note != "" {
				fmt.Println(note)
			}
			return runSync(confirmDeletes)
		},
	}
	c.Flags().IntVar(&confirmDeletes, "confirm-deletes", 0, "allow exactly this many remote deletions (printed when needed)")
	return c
}

func runSync(confirmDeletes int) error {
	unlock, err := lock()
	if err != nil {
		return err
	}
	defer unlock()

	cfg, s, err := loadSyncer()
	if err != nil {
		return err
	}
	ctx := context.Background()
	plan, err := s.BuildPlan(ctx)
	if err != nil {
		return err
	}
	transfers := plan.Count(sync.ActUpload) + plan.Count(sync.ActDownload) + plan.Count(sync.ActConflict) + plan.Count(sync.ActVerify)
	start := time.Now()
	last := time.Now()
	opts := sync.Options{
		ConfirmDeletes: confirmDeletes,
		Confirm: func(n int, p *sync.Plan) bool {
			printDeletions(p)
			if !interactive() {
				return false
			}
			return ask(fmt.Sprintf("Type %d to delete these %d remote files (trash copy kept), or press Enter to skip deletions: ", n, n)) == strconv.Itoa(n)
		},
		Progress: func(done, total int) {
			if total >= 50 && (time.Since(last) > 3*time.Second || done == total) {
				last = time.Now()
				fmt.Fprintf(os.Stderr, "  %d/%d files transferred\n", done, total)
			}
		},
	}
	if transfers >= 200 {
		fmt.Printf("%d files to transfer; this can take a few minutes.\n", transfers)
	}
	res, err := s.Execute(ctx, plan, opts)
	if err != nil {
		return err
	}
	if cfg.IsMCPSyncEnabled() {
		if _, err := s.PushMCP(ctx); err != nil {
			res.Errors = append(res.Errors, err)
		}
		if _, err := s.PullMCP(ctx); err != nil {
			res.Errors = append(res.Errors, err)
		}
	}
	printResult(res, time.Since(start))
	if len(res.Errors) > 0 {
		return fmt.Errorf("%d file(s) had errors (listed above); they retry on the next sync", len(res.Errors))
	}
	return nil
}

func printResult(r *sync.Result, took time.Duration) {
	fmt.Printf("claude-sync: up %d, down %d, deleted %d, conflicts %d, too large %d  (%s)\n",
		len(r.Uploaded), len(r.Downloaded), len(r.Deleted), len(r.KeptBoth)+len(r.Merged), len(r.TooLarge), took.Round(time.Second))
	if verbose {
		list("up", r.Uploaded)
		list("down", r.Downloaded)
	}
	list("deleted remotely (trash kept)", r.Deleted)
	for _, p := range r.KeptBoth {
		fmt.Printf("  KEPT BOTH: %s\n    remote copy: %s\n    review, merge into your file, then delete the copy; the next sync uploads yours.\n", p, r.Copies[p])
	}
	for _, p := range r.Merged {
		if c, ok := r.Copies[p]; ok {
			fmt.Printf("  transcript diverged, kept local: %s (remote copy kept at %s, not synced)\n", p, c)
		} else if verbose {
			fmt.Printf("  auto-merged: %s\n", p)
		}
	}
	list("still frozen (delete the .conflict copy to resolve)", r.Frozen)
	list("too large to sync (over 95 MB encrypted)", r.TooLarge)
	if len(r.Tombstoned) > 0 {
		fmt.Printf("  %d file(s) were deleted on another machine and are kept here untouched (edit one to re-share it)\n", len(r.Tombstoned))
		if verbose {
			list("", r.Tombstoned)
		}
	}
	if r.HeldDeletes > 0 {
		fmt.Printf("  HELD %d remote deletions (over the safety limit). Review with `claude-sync plan`, then run `claude-sync --confirm-deletes %d` to allow exactly those.\n", r.HeldDeletes, r.HeldDeletes)
	}
	if r.HistoryRebuilt && r.HistoryAdded > 0 {
		fmt.Printf("  history.jsonl rebuilt (+%d entries)\n", r.HistoryAdded)
	}
	if r.Forgotten > 0 {
		fmt.Printf("  stopped tracking %d path(s) that the rules no longer cover (nothing deleted)\n", r.Forgotten)
	}
	for _, e := range r.Errors {
		fmt.Printf("  error: %s\n", scrub(e.Error()))
	}
}

func list(label string, paths []string) {
	if len(paths) == 0 {
		return
	}
	if label != "" {
		fmt.Printf("  %s:\n", label)
	}
	for _, p := range paths {
		fmt.Printf("    %s\n", p)
	}
}

// ---------------------------------------------------------------------------
// plan

func planCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "plan",
		Short: "Show what a sync would do, with counts; changes nothing",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, s, err := loadSyncer()
			if err != nil {
				return err
			}
			p, err := s.BuildPlan(context.Background())
			if err != nil {
				return err
			}
			printPlan(p)
			return nil
		},
	}
}

func printPlan(p *sync.Plan) {
	fmt.Println("Plan (read-only, nothing has changed):")
	row := func(label string, a sync.Action, note string) {
		fmt.Printf("  %-26s %6d  %s\n", label, p.Count(a), note)
	}
	row("upload", sync.ActUpload, "")
	row("download", sync.ActDownload, "")
	row("delete remote", sync.ActDeleteRemote, "trash copy kept 30 days")
	row("both changed", sync.ActConflict, "auto-merge or keep both")
	row("first-contact check", sync.ActVerify, "download and compare; differences kept both")
	row("record baseline only", sync.ActRecord, "already identical")
	row("too large (skipped)", sync.ActTooLarge, "")
	row("frozen conflicts", sync.ActFrozen, "")
	row("deleted elsewhere, kept", sync.ActTombstoned, "")
	inSync := len(p.Items)
	for _, a := range []sync.Action{sync.ActUpload, sync.ActDownload, sync.ActDeleteRemote, sync.ActConflict, sync.ActVerify, sync.ActRecord, sync.ActTooLarge, sync.ActFrozen, sync.ActTombstoned, sync.ActDrop} {
		inSync -= p.Count(a)
	}
	fmt.Printf("  %-26s %6d\n", "already in sync", inSync)
	if p.OutOfScope > 0 {
		fmt.Printf("  %-26s %6d  left alone; `claude-sync prune` to review\n", "remote, outside the rules", p.OutOfScope)
	}
	if p.Forgotten > 0 {
		fmt.Printf("  %-26s %6d  no longer covered by rules; forgotten, nothing deleted\n", "stale baselines", p.Forgotten)
	}
	if len(p.Collisions) > 0 {
		fmt.Printf("  case collisions skipped: %s\n", strings.Join(p.Collisions, ", "))
	}
	if len(p.UnknownToken) > 0 {
		fmt.Printf("  %d remote keys use an unknown v1 path_map token and are skipped\n", len(p.UnknownToken))
	}
	if p.Count(sync.ActDeleteRemote) > 0 {
		printDeletions(p)
	}
	if verbose {
		for _, a := range []sync.Action{sync.ActUpload, sync.ActDownload, sync.ActConflict, sync.ActVerify, sync.ActTooLarge} {
			list(string(a), p.Paths(a))
		}
	}
}

func printDeletions(p *sync.Plan) {
	paths := p.Paths(sync.ActDeleteRemote)
	if len(paths) <= 25 {
		list("remote deletions (deleted on this machine)", paths)
		return
	}
	dirs := map[string]int{}
	for _, x := range paths {
		d := path.Dir(x)
		if parts := strings.SplitN(x, "/", 3); len(parts) == 3 {
			d = parts[0] + "/" + parts[1]
		}
		dirs[d]++
	}
	type kv struct {
		k string
		v int
	}
	var kvs []kv
	for k, v := range dirs {
		kvs = append(kvs, kv{k, v})
	}
	sort.Slice(kvs, func(i, j int) bool { return kvs[i].v > kvs[j].v })
	fmt.Printf("  remote deletions: %d files, top folders:\n", len(paths))
	for i, e := range kvs {
		if i == 10 {
			break
		}
		fmt.Printf("    %5d  %s\n", e.v, e.k)
	}
}

// ---------------------------------------------------------------------------
// setup

func setupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "setup",
		Short: "Configure storage and the encryption passphrase on this machine",
		Long: "Asks for the R2 account ID, bucket, access key, secret and passphrase.\n" +
			"Any answer can be an op://vault/item/field reference; it is read with\n" +
			"`op read` straight into memory and never shown or written anywhere else.",
		RunE: func(cmd *cobra.Command, args []string) error { return runSetup() },
	}
}

func runSetup() error {
	if !interactive() {
		return errors.New("setup needs a terminal: run it yourself, not through an agent")
	}
	if config.Exists() && !confirm("This machine already has a config. Replace it?") {
		return nil
	}
	fmt.Println("claude-sync setup. Secret answers are hidden; any answer may be an op:// reference.")
	accountID, err := prompt("Cloudflare account ID", false)
	if err != nil {
		return err
	}
	bucket, err := prompt("Bucket [claude-sync]", false)
	if err != nil {
		return err
	}
	if bucket == "" {
		bucket = "claude-sync"
	}
	accessKey, err := prompt("R2 access key ID", true)
	if err != nil {
		return err
	}
	secretKey, err := prompt("R2 secret access key", true)
	if err != nil {
		return err
	}
	pass, err := prompt("Encryption passphrase (same on every machine)", true)
	if err != nil {
		return err
	}

	sc := &storage.StorageConfig{
		Provider: storage.ProviderR2, Bucket: bucket, AccountID: accountID,
		AccessKeyID: accessKey, SecretAccessKey: secretKey,
	}
	keyPath := config.AgeKeyFilePath()
	if err := os.MkdirAll(config.ConfigDirPath(), 0700); err != nil {
		return err
	}
	tmpKey := keyPath + ".setup-tmp"
	defer func() { _ = os.Remove(tmpKey) }()
	if err := crypto.GenerateKeyFromPassphrase(tmpKey, pass); err != nil {
		return err
	}
	cfg := &config.Config{Storage: sc, EncryptionKey: tmpKey}
	s, err := sync.NewSyncer(cfg, true)
	if err != nil {
		return err
	}
	fmt.Println("Checking the bucket and passphrase...")
	empty, err := s.VerifyKey(context.Background())
	if err != nil {
		return fmt.Errorf("%s. Nothing was saved", scrubWith(err.Error(), accessKey, secretKey))
	}
	if empty {
		if err := crypto.ValidatePassphraseStrength(pass); err != nil {
			return err
		}
		again, err := prompt("Bucket is empty: confirm the new passphrase", true)
		if err != nil {
			return err
		}
		if again != pass {
			return errors.New("passphrases differ; nothing was saved")
		}
	}

	if config.Exists() {
		if _, err := config.BackupFile(config.ConfigFilePath(), "pre-setup"); err != nil {
			return err
		}
		if _, err := config.BackupFile(keyPath, "pre-setup"); err != nil {
			return err
		}
	}
	keyData, err := os.ReadFile(tmpKey)
	if err != nil {
		return err
	}
	if err := config.WriteFileAtomic(keyPath, keyData); err != nil {
		return err
	}
	cfg.EncryptionKey = keyPath
	if err := config.Save(cfg); err != nil {
		return err
	}
	fmt.Println("Saved config and key in", config.ConfigDirPath(), "(outside ~/.claude, never synced).")

	_, s, err = loadSyncer()
	if err != nil {
		return err
	}
	p, err := s.BuildPlan(context.Background())
	if err != nil {
		return err
	}
	printPlan(p)
	if confirm("Run the first sync now?") {
		return runSync(0)
	}
	fmt.Println("Run `claude-sync` when ready.")
	return nil
}

func scrubWith(s string, secrets ...string) string {
	for _, x := range secrets {
		if len(x) >= 8 {
			s = strings.ReplaceAll(s, x, "[redacted]")
		}
	}
	return s
}

// prompt reads one answer. op:// references are resolved with `op read`.
func prompt(label string, secret bool) (string, error) {
	fmt.Print(label + ": ")
	var v string
	if secret {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if err != nil {
			return "", err
		}
		v = strings.TrimSpace(string(b))
	} else {
		line, _ := stdin.ReadString('\n')
		v = strings.TrimSpace(line)
	}
	if strings.HasPrefix(v, "op://") {
		return opRead(v)
	}
	return v, nil
}

// opRead resolves a 1Password reference into memory. If a service-account
// token in the environment cannot see the vault, it retries with the token
// removed so the user's own signed-in session is used.
func opRead(ref string) (string, error) {
	run := func(env []string) (string, error) {
		cmd := exec.Command("op", "read", "--no-newline", ref)
		cmd.Env = env
		cmd.Stderr = nil
		out, err := cmd.Output()
		return string(out), err
	}
	out, err := run(os.Environ())
	if err != nil && os.Getenv("OP_SERVICE_ACCOUNT_TOKEN") != "" {
		var env []string
		for _, e := range os.Environ() {
			if !strings.HasPrefix(e, "OP_SERVICE_ACCOUNT_TOKEN=") {
				env = append(env, e)
			}
		}
		out, err = run(env)
	}
	if err != nil {
		return "", fmt.Errorf("op read failed for that reference (signed in to 1Password?)")
	}
	return strings.TrimSpace(out), nil
}

// ---------------------------------------------------------------------------
// migrate

func migrateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "migrate",
		Short: "One-time upgrade of this machine's v1 config and state (safe to re-run)",
		RunE: func(cmd *cobra.Command, args []string) error {
			unlock, err := lock()
			if err != nil {
				return err
			}
			defer unlock()

			cfg, err := config.Load()
			if err != nil {
				return errors.New("no v1 config found on this machine; run `claude-sync setup` instead")
			}
			for _, f := range []string{config.ConfigFilePath(), config.StateFilePath()} {
				b, err := config.BackupFile(f, "v1")
				if err != nil {
					return fmt.Errorf("backup failed: %w", err)
				}
				if b != "" {
					fmt.Println("backed up", path.Base(f), "->", b)
				}
			}
			if cfg.HasV1Rules() {
				fmt.Println("config.yaml: v1 sync_paths/exclude left in place for any old binary; v2 ignores them (rules are built in)")
			}
			_, s, err := loadSyncer()
			if err != nil {
				return err
			}
			ctx := context.Background()
			fmt.Println("checking the key against the bucket...")
			if _, err := s.VerifyKey(ctx); err != nil {
				return err
			}
			files, lines, err := s.RepairWindowsTranscripts()
			if err != nil {
				return err
			}
			if files > 0 {
				fmt.Printf("repaired %d transcript line(s) in %d file(s) broken by v1 on Windows\n", lines, files)
			}
			s.GetState().Schema = sync.StateSchema
			if err := s.GetState().Save(); err != nil {
				return err
			}
			p, err := s.BuildPlan(ctx)
			if err != nil {
				return err
			}
			printPlan(p)
			fmt.Println("migrate done. Nothing was uploaded or downloaded.")
			return nil
		},
	}
}

// ---------------------------------------------------------------------------
// conflicts, rules, prune, history, version

func conflictsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "conflicts",
		Short: "List unresolved keep-both conflicts",
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := sync.LoadState()
			if err != nil {
				return err
			}
			if len(st.Conflicts) == 0 {
				fmt.Println("no unresolved conflicts")
				return nil
			}
			claudeDir := config.ClaudeDir()
			for p, c := range st.Conflicts {
				fmt.Printf("%s\n  remote copy: %s\n", p, c.Copy)
			}
			fmt.Printf("Merge what you want into the file, delete the copy (under %s), then run claude-sync.\n", claudeDir)
			return nil
		},
	}
}

func rulesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rules",
		Short: "Print the built-in sync rules",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("rules v%d (built into this binary; the same on every machine)\n", rules.Version)
			fmt.Println("synced under ~/.claude:")
			for _, p := range rules.Paths {
				fmt.Println("  " + p)
			}
			fmt.Println("excluded everywhere:")
			for _, p := range rules.Excludes {
				fmt.Println("  " + p)
			}
			fmt.Printf("size limit: files over %d MB encrypted are skipped with a warning\n", rules.MaxUploadBytes/1024/1024)
			fmt.Println("project paths under ~/Code are rewritten so sessions resume across macOS and Windows")
		},
	}
}

func pruneCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "prune",
		Short: "Review and delete remote objects outside the rules, and trash older than 30 days",
		RunE: func(cmd *cobra.Command, args []string) error {
			unlock, err := lock()
			if err != nil {
				return err
			}
			defer unlock()
			_, s, err := loadSyncer()
			if err != nil {
				return err
			}
			ctx := context.Background()
			pp, err := s.BuildPrune(ctx)
			if err != nil {
				return err
			}
			n := len(pp.OutOfScope) + len(pp.OldTrash)
			if n == 0 {
				fmt.Println("nothing to prune")
				return nil
			}
			tops := map[string]int{}
			for _, k := range pp.OutOfScope {
				tops[strings.SplitN(k, "/", 2)[0]]++
			}
			fmt.Printf("outside the rules: %d objects; old trash: %d objects; %.1f MB total\n", len(pp.OutOfScope), len(pp.OldTrash), float64(pp.Bytes)/1e6)
			for k, v := range tops {
				fmt.Printf("  %6d  %s\n", v, k)
			}
			if verbose {
				list("outside the rules", pp.OutOfScope)
			}
			if !interactive() {
				fmt.Println("prune needs a terminal to confirm; run it yourself.")
				return nil
			}
			if ask(fmt.Sprintf("Type %d to delete them (out-of-scope objects get a trash copy first): ", n)) != strconv.Itoa(n) {
				fmt.Println("nothing deleted")
				return nil
			}
			return s.ExecutePrune(ctx, pp)
		},
	}
}

func rebuildHistoryCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rebuild-history",
		Short: "Rebuild history.jsonl from session files (sync does this automatically)",
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := sync.RebuildHistory(config.ClaudeDir())
			if err != nil {
				return err
			}
			fmt.Printf("history.jsonl: %d existing, %d recovered, %d written\n", r.Existing, r.Reconstructed, r.Merged)
			return nil
		},
	}
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version and rules version",
		Run:   func(cmd *cobra.Command, args []string) { fmt.Println("claude-sync", versionString()) },
	}
}

func removedCmd(name, msg string) *cobra.Command {
	return &cobra.Command{
		Use:                name,
		Hidden:             true,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("`claude-sync %s` %s", name, msg)
		},
	}
}

// ---------------------------------------------------------------------------
// mcp (opt-in, unchanged behavior)

func mcpCmd() *cobra.Command {
	c := &cobra.Command{Use: "mcp", Short: "Sync MCP server configs from ~/.claude.json (opt-in)"}
	withSyncer := func(fn func(context.Context, *config.Config, *sync.Syncer) error) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, args []string) error {
			cfg, s, err := loadSyncer()
			if err != nil {
				return err
			}
			return fn(context.Background(), cfg, s)
		}
	}
	c.AddCommand(
		&cobra.Command{Use: "status", Short: "Show MCP sync state", RunE: withSyncer(func(ctx context.Context, cfg *config.Config, s *sync.Syncer) error {
			st, err := s.MCPStatus(ctx)
			if err != nil {
				return err
			}
			fmt.Printf("auto-sync: %v, servers: %d, unpushed changes: %v\n", cfg.IsMCPSyncEnabled(), st.ServerCount, st.HasChanges)
			return nil
		})},
		&cobra.Command{Use: "enable", Short: "Include MCP configs in every sync", RunE: withSyncer(func(ctx context.Context, cfg *config.Config, s *sync.Syncer) error {
			cfg.SetMCPSync(true)
			return config.Save(cfg)
		})},
		&cobra.Command{Use: "disable", Short: "Stop including MCP configs in sync", RunE: withSyncer(func(ctx context.Context, cfg *config.Config, s *sync.Syncer) error {
			cfg.SetMCPSync(false)
			return config.Save(cfg)
		})},
		&cobra.Command{Use: "push", Short: "Upload MCP configs now", RunE: withSyncer(func(ctx context.Context, cfg *config.Config, s *sync.Syncer) error {
			r, err := s.PushMCP(ctx)
			if err == nil {
				fmt.Printf("MCP: %d servers pushed (unchanged: %v)\n", r.ServersPushed, r.Unchanged)
			}
			return err
		})},
		&cobra.Command{Use: "pull", Short: "Merge remote MCP configs now", RunE: withSyncer(func(ctx context.Context, cfg *config.Config, s *sync.Syncer) error {
			r, err := s.PullMCP(ctx)
			if err == nil {
				fmt.Printf("MCP: %d added, %d updated, %d conflicts kept local\n", len(r.Added), len(r.Updated), len(r.Conflicts))
			}
			return err
		})},
	)
	return c
}

// ---------------------------------------------------------------------------
// lock: one claude-sync at a time per machine.

func lock() (func(), error) {
	if err := os.MkdirAll(config.ConfigDirPath(), 0700); err != nil {
		return nil, err
	}
	p := config.ConfigDirPath() + string(os.PathSeparator) + "sync.lock"
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err == nil {
			fmt.Fprintf(f, "%d %s\n", os.Getpid(), time.Now().Format(time.RFC3339))
			_ = f.Close()
			return func() { _ = os.Remove(p) }, nil
		}
		if fi, serr := os.Stat(p); serr == nil && time.Since(fi.ModTime()) > 2*time.Hour {
			_ = os.Remove(p) // stale lock from a crashed run
			continue
		}
		break
	}
	return nil, fmt.Errorf("another claude-sync is running on this machine (lock: %s; delete it if no sync is running)", p)
}
