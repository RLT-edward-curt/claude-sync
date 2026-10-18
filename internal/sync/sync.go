package sync

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tawanorg/claude-sync/internal/config"
	"github.com/tawanorg/claude-sync/internal/crypto"
	"github.com/tawanorg/claude-sync/internal/rules"
	"github.com/tawanorg/claude-sync/internal/storage"

	// Register storage adapters
	_ "github.com/tawanorg/claude-sync/internal/storage/gcs"
	_ "github.com/tawanorg/claude-sync/internal/storage/r2"
	_ "github.com/tawanorg/claude-sync/internal/storage/s3"
)

const defaultWorkers = 16

// maxDecompressedSize bounds gzip expansion (decompression bomb guard).
const maxDecompressedSize = 500 * 1024 * 1024

// Remote metadata keys. Everything under _metadata/ is skipped by v1 pulls,
// so v2-only metadata lives there in separate keys.
const (
	ManifestKey   = "_metadata/manifest.json" // + ".age"; same shape v1 writes
	TombstonesKey = "_metadata/tombstones.json.age"
	TrashPrefix   = "_metadata/trash/"
)

// Deletion guard: more remote deletions than this (or than DeleteGuardPct of
// tracked files) in one sync needs explicit confirmation.
const (
	DeleteGuardCount = 20
	DeleteGuardPct   = 2
)

// FileManifest stores metadata about synced files, primarily mtimes.
type FileManifest struct {
	Files map[string]FileMetadata `json:"files"`
}

// FileMetadata stores metadata for a single file.
type FileMetadata struct {
	ModTime time.Time `json:"mod_time"`
}

// Tombstone records a deliberate deletion so other v2 machines do not
// resurrect the file by self-healing.
type Tombstone struct {
	DeletedAt time.Time `json:"deleted_at"`
	Device    string    `json:"device"`
}

type Syncer struct {
	storage   storage.Storage
	encryptor *crypto.Encryptor
	state     *SyncState
	claudeDir string
	cfg       *config.Config
	paths     *PathMapper
	device    string
	quiet     bool
}

// NewSyncer builds a syncer from the loaded config.
func NewSyncer(cfg *config.Config, quiet bool) (*Syncer, error) {
	store, err := storage.New(cfg.GetStorageConfig())
	if err != nil {
		return nil, fmt.Errorf("failed to create storage client: %w", err)
	}
	enc, err := crypto.NewEncryptor(cfg.EncryptionKey)
	if err != nil {
		return nil, fmt.Errorf("failed to load encryption key: %w", err)
	}
	var state *SyncState
	if cfg.StateDirOverride != "" {
		state, err = LoadStateFromDir(cfg.StateDirOverride)
	} else {
		state, err = LoadState()
	}
	if err != nil {
		return nil, fmt.Errorf("failed to load state: %w", err)
	}
	claudeDir := config.ClaudeDir()
	if cfg.ClaudeDirOverride != "" {
		claudeDir = cfg.ClaudeDirOverride
	}
	homeDir, _ := os.UserHomeDir()
	return NewSyncerWith(cfg, store, enc, state, claudeDir, homeDir, runtime.GOOS == "windows", quiet), nil
}

// NewSyncerWith creates a Syncer with explicit dependencies (tests simulate
// other machines and Windows through homeDir and windows).
func NewSyncerWith(cfg *config.Config, store storage.Storage, enc *crypto.Encryptor, state *SyncState, claudeDir, homeDir string, windows, quiet bool) *Syncer {
	host, _ := os.Hostname()
	return &Syncer{
		storage:   store,
		encryptor: enc,
		state:     state,
		claudeDir: claudeDir,
		cfg:       cfg,
		paths:     NewPathMapperFor(homeDir, windows),
		device:    host,
		quiet:     quiet,
	}
}

func (s *Syncer) GetState() *SyncState { return s.state }

// ClaudeDir is the synced directory.
func (s *Syncer) ClaudeDir() string { return s.claudeDir }

func (s *Syncer) log(format string, args ...interface{}) {
	if !s.quiet {
		fmt.Printf(format+"\n", args...)
	}
}

// ---------------------------------------------------------------------------
// Planning

// Action is what a sync will do for one path.
type Action string

const (
	ActUpload       Action = "upload"
	ActDownload     Action = "download"
	ActDeleteRemote Action = "delete-remote"
	ActConflict     Action = "conflict"   // both changed, resolved per rules.ClassOf
	ActVerify       Action = "verify"     // both exist with no baseline: compare content
	ActRecord       Action = "record"     // already in sync; refresh baseline only
	ActFrozen       Action = "frozen"     // unresolved keep-both conflict
	ActTombstoned   Action = "tombstoned" // deleted elsewhere, kept here
	ActTooLarge     Action = "too-large"
	ActDrop         Action = "drop" // forget a stale baseline, touch nothing
)

// Item is one planned operation.
type Item struct {
	Path   string
	Action Action
	Remote *storage.ObjectInfo
	Local  *localFile
}

type localFile struct {
	Size    int64
	ModTime time.Time
	Hash    string
	Mode    os.FileMode
}

// Plan is the full reconcile decision for one sync.
type Plan struct {
	Items        []Item
	Forgotten    int // baselines dropped because the path is out of scope
	OutOfScope   int // remote objects outside the rules (left alone; see prune)
	Collisions   []string
	UnknownToken []string
	Tracked      int
	tombstones   map[string]Tombstone
	remoteKeys   map[string]storage.ObjectInfo
	manifestSeen bool
}

// Count returns how many items have the given action.
func (p *Plan) Count(a Action) int {
	n := 0
	for _, it := range p.Items {
		if it.Action == a {
			n++
		}
	}
	return n
}

// Paths returns the paths with the given action, sorted.
func (p *Plan) Paths(a Action) []string {
	var out []string
	for _, it := range p.Items {
		if it.Action == a {
			out = append(out, it.Path)
		}
	}
	sort.Strings(out)
	return out
}

// DeletionsNeedConfirm reports whether the remote deletions exceed the guard.
func (p *Plan) DeletionsNeedConfirm() bool {
	n := p.Count(ActDeleteRemote)
	if n > DeleteGuardCount {
		return true
	}
	return p.Tracked > 0 && n*100 > p.Tracked*DeleteGuardPct && n > 0 && n > 5
}

func (s *Syncer) remoteKey(relPath string) string {
	return s.paths.NormalizeRelPath(relPath) + ".age"
}

// BuildPlan lists the bucket, scans ~/.claude and decides every path. It
// changes nothing.
func (s *Syncer) BuildPlan(ctx context.Context) (*Plan, error) {
	objects, err := s.storage.List(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("failed to list remote objects: %w", err)
	}
	plan := &Plan{remoteKeys: make(map[string]storage.ObjectInfo)}

	// Remote side: local path -> object.
	remote := make(map[string]storage.ObjectInfo)
	lower := make(map[string]string)
	for _, obj := range objects {
		obj.ETag = storage.NormalizeETag(obj.ETag)
		plan.remoteKeys[obj.Key] = obj
		if !strings.HasSuffix(obj.Key, ".age") ||
			strings.HasPrefix(obj.Key, "_metadata/") || strings.HasPrefix(obj.Key, "_external/") {
			continue
		}
		local, ok := s.paths.ResolveRelPath(strings.TrimSuffix(obj.Key, ".age"))
		if !ok {
			plan.UnknownToken = append(plan.UnknownToken, obj.Key)
			continue
		}
		if !rules.InScope(local) {
			plan.OutOfScope++
			continue
		}
		if existing, dup := remote[local]; dup {
			// A legacy un-normalized key and its canonical form both exist:
			// the canonical one wins.
			if existing.Key == s.remoteKey(local) {
				continue
			}
		}
		lk := strings.ToLower(local)
		if prev, clash := lower[lk]; clash && prev != local && runtime.GOOS != "linux" {
			plan.Collisions = append(plan.Collisions, local)
			continue
		}
		lower[lk] = local
		remote[local] = obj
	}
	_, plan.manifestSeen = plan.remoteKeys[ManifestKey+".age"]

	tombs, err := s.loadTombstones(ctx, plan.remoteKeys)
	if err != nil {
		return nil, err
	}
	plan.tombstones = tombs

	local, err := s.scanLocal()
	if err != nil {
		return nil, err
	}

	// Drop baselines for paths no longer in scope: never a remote delete.
	s.state.mu.Lock()
	var baselinePaths []string
	for p := range s.state.Files {
		if strings.HasPrefix(p, "_external/") {
			continue
		}
		if !rules.InScope(p) {
			plan.Forgotten++
			continue
		}
		baselinePaths = append(baselinePaths, p)
	}
	plan.Tracked = len(baselinePaths)
	s.state.mu.Unlock()

	all := make(map[string]struct{})
	for p := range local {
		all[p] = struct{}{}
	}
	for p := range remote {
		all[p] = struct{}{}
	}
	for _, p := range baselinePaths {
		all[p] = struct{}{}
	}

	names := make([]string, 0, len(all))
	for p := range all {
		names = append(names, p)
	}
	sort.Strings(names)

	for _, p := range names {
		var lf *localFile
		if l, ok := local[p]; ok {
			lf = l
		}
		var ro *storage.ObjectInfo
		if r, ok := remote[p]; ok {
			r := r
			ro = &r
		}
		act := s.decide(p, lf, ro, s.state.GetFile(p), tombs)
		plan.Items = append(plan.Items, Item{Path: p, Action: act, Remote: ro, Local: lf})
	}
	return plan, nil
}

// decide applies the reconcile table from docs/RLT-REBUILD-DESIGN.md, then
// the size limits.
func (s *Syncer) decide(p string, l *localFile, r *storage.ObjectInfo, b *FileState, tombs map[string]Tombstone) Action {
	if c, ok := s.state.Conflicts[p]; ok {
		if _, err := os.Stat(filepath.Join(s.claudeDir, filepath.FromSlash(c.Copy))); err == nil {
			return ActFrozen
		}
	}
	act := decideTable(s, p, l, r, b, tombs)
	switch act {
	case ActDownload, ActConflict, ActVerify:
		if r.Size > int64(rules.MaxDownloadBytes) {
			return ActTooLarge
		}
	}
	if act == ActUpload {
		s.state.mu.Lock()
		h, big := s.state.TooLarge[p]
		s.state.mu.Unlock()
		if big && h == l.Hash {
			return ActTooLarge
		}
	}
	return act
}

func decideTable(s *Syncer, p string, l *localFile, r *storage.ObjectInfo, b *FileState, tombs map[string]Tombstone) Action {
	switch {
	case l != nil && r != nil && b != nil:
		lc, rc := l.Hash != b.Hash, remoteChanged(b, r)
		switch {
		case lc && rc:
			return ActConflict
		case lc:
			return ActUpload
		case rc:
			return ActDownload
		case b.RemoteETag != r.ETag:
			return ActRecord
		}
		return ""
	case l != nil && r != nil:
		return ActVerify
	case l != nil:
		if t, ok := tombs[s.remoteKey(p)]; ok && !l.ModTime.After(t.DeletedAt) {
			return ActTombstoned
		}
		return ActUpload
	case r != nil && b != nil:
		if remoteChanged(b, r) {
			return ActDownload
		}
		return ActDeleteRemote
	case r != nil:
		return ActDownload
	case b != nil:
		return ActDrop
	}
	return ""
}

// remoteChanged compares the listed object against the baseline. v1 baselines
// carry no ETag; for those, v1's own rule applies once (remote modified after
// our last sync of the file), and the next sync records the ETag.
func remoteChanged(b *FileState, r *storage.ObjectInfo) bool {
	if b.RemoteETag != "" {
		return b.RemoteETag != r.ETag
	}
	return r.LastModified.After(b.Uploaded)
}

// scanLocal walks the in-scope paths. Hashes are reused from the baseline
// when size and mtime are unchanged.
func (s *Syncer) scanLocal() (map[string]*localFile, error) {
	out := make(map[string]*localFile)
	add := func(rel string, fi os.FileInfo) error {
		if !fi.Mode().IsRegular() || !rules.InScope(rel) {
			return nil
		}
		lf := &localFile{Size: fi.Size(), ModTime: fi.ModTime(), Mode: fi.Mode().Perm()}
		if b := s.state.GetFile(rel); b != nil && b.Size == lf.Size && b.ModTime.Equal(lf.ModTime) && b.Hash != "" {
			lf.Hash = b.Hash
		} else {
			h, err := HashFile(filepath.Join(s.claudeDir, filepath.FromSlash(rel)))
			if err != nil {
				return nil // unreadable right now (e.g. locked): skip this sync
			}
			lf.Hash = h
		}
		out[rel] = lf
		return nil
	}
	for _, top := range rules.Paths {
		full := filepath.Join(s.claudeDir, top)
		fi, err := os.Lstat(full)
		if err != nil {
			continue
		}
		if !fi.IsDir() {
			if err := add(top, fi); err != nil {
				return nil, err
			}
			continue
		}
		err = filepath.Walk(full, func(path string, fi os.FileInfo, err error) error {
			if err != nil {
				return nil // vanished or unreadable entry: skip
			}
			rel, _ := filepath.Rel(s.claudeDir, path)
			rel = filepath.ToSlash(rel)
			if fi.IsDir() {
				if rel != top && rules.Excluded(rel) {
					return filepath.SkipDir
				}
				return nil
			}
			return add(rel, fi)
		})
		if err != nil {
			return nil, fmt.Errorf("failed to scan %s: %w", top, err)
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Execution

// Options control one sync run.
type Options struct {
	// ConfirmDeletes allows exactly this many remote deletions when the
	// count exceeds the guard. Zero means "hold them".
	ConfirmDeletes int
	// Confirm is called when deletions exceed the guard and ConfirmDeletes
	// does not match; it returns true to proceed. Nil means hold.
	Confirm func(n int, plan *Plan) bool
	// Progress, if set, is called after each transfer.
	Progress func(done, total int)
}

// Result reports what a sync did.
type Result struct {
	Uploaded       []string
	Downloaded     []string
	Deleted        []string
	Recorded       int
	KeptBoth       []string // new keep-both conflicts: path -> copy in Copies
	Copies         map[string]string
	Merged         []string // history / transcript auto-resolved
	Frozen         []string
	Tombstoned     []string
	TooLarge       []string
	HeldDeletes    int
	Forgotten      int
	OutOfScope     int
	HistoryRebuilt bool
	HistoryAdded   int
	Errors         []error
}

// Sync plans and executes one full reconcile.
func (s *Syncer) Sync(ctx context.Context, opts Options) (*Result, error) {
	plan, err := s.BuildPlan(ctx)
	if err != nil {
		return nil, err
	}
	return s.Execute(ctx, plan, opts)
}

// Execute carries out a plan.
func (s *Syncer) Execute(ctx context.Context, plan *Plan, opts Options) (*Result, error) {
	res := &Result{Copies: map[string]string{}, Forgotten: plan.Forgotten, OutOfScope: plan.OutOfScope}
	var mu sync.Mutex
	addErr := func(p string, err error) {
		mu.Lock()
		res.Errors = append(res.Errors, fmt.Errorf("%s: %w", p, err))
		mu.Unlock()
	}

	// Forget out-of-scope baselines.
	s.state.mu.Lock()
	for p := range s.state.Files {
		if !strings.HasPrefix(p, "_external/") && !rules.InScope(p) {
			delete(s.state.Files, p)
		}
	}
	for p, c := range s.state.Conflicts {
		if _, err := os.Stat(filepath.Join(s.claudeDir, filepath.FromSlash(c.Copy))); err != nil {
			delete(s.state.Conflicts, p) // copy deleted: conflict resolved, local wins
		}
	}
	s.state.mu.Unlock()

	allowDeletes := true
	nDel := plan.Count(ActDeleteRemote)
	if plan.DeletionsNeedConfirm() && opts.ConfirmDeletes != nDel {
		allowDeletes = opts.Confirm != nil && opts.Confirm(nDel, plan)
		if !allowDeletes {
			res.HeldDeletes = nDel
		}
	}

	var work []Item
	var deletes []Item
	for _, it := range plan.Items {
		switch it.Action {
		case ActDeleteRemote:
			if allowDeletes {
				deletes = append(deletes, it)
			}
		case ActFrozen:
			res.Frozen = append(res.Frozen, it.Path)
		case ActTombstoned:
			res.Tombstoned = append(res.Tombstoned, it.Path)
			s.state.RemoveFile(it.Path)
		case ActTooLarge:
			res.TooLarge = append(res.TooLarge, it.Path)
		case ActDrop:
			s.state.RemoveFile(it.Path)
		case ActRecord:
			s.state.SetFile(it.Path, it.Local.Hash, it.Local.Size, it.Local.ModTime, it.Remote.ETag)
			res.Recorded++
		case ActUpload, ActDownload, ActConflict, ActVerify:
			work = append(work, it)
		}
	}

	var manifest *FileManifest
	if plan.manifestSeen {
		manifest, _ = s.downloadManifest(ctx)
	}

	// Transfers run concurrently.
	total := len(work)
	done := 0
	sem := make(chan struct{}, defaultWorkers)
	var wg sync.WaitGroup
	tombsCleared := make(map[string]bool)
	for _, it := range work {
		wg.Add(1)
		go func(it Item) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			var err error
			switch it.Action {
			case ActUpload:
				var tooBig bool
				tooBig, err = s.upload(ctx, it.Path)
				mu.Lock()
				if err == nil && tooBig {
					res.TooLarge = append(res.TooLarge, it.Path)
				} else if err == nil {
					res.Uploaded = append(res.Uploaded, it.Path)
					if _, ok := plan.tombstones[s.remoteKey(it.Path)]; ok {
						tombsCleared[s.remoteKey(it.Path)] = true
					}
				}
				mu.Unlock()
			case ActDownload:
				err = s.download(ctx, it.Path, it.Remote, manifest)
				if err == nil {
					mu.Lock()
					res.Downloaded = append(res.Downloaded, it.Path)
					mu.Unlock()
				}
			case ActConflict, ActVerify:
				err = s.resolve(ctx, it, manifest, res, &mu)
			}
			if err != nil {
				addErr(it.Path, err)
			}
			mu.Lock()
			done++
			if opts.Progress != nil {
				opts.Progress(done, total)
			}
			mu.Unlock()
		}(it)
	}
	wg.Wait()

	// Deletions: trash copy, tombstone, delete.
	newTombs := make(map[string]Tombstone)
	if len(deletes) > 0 {
		day := time.Now().UTC().Format("20060102")
		var keys []string
		for _, it := range deletes {
			key := it.Remote.Key
			data, err := s.storage.Download(ctx, key)
			if err == nil {
				err = s.storage.Upload(ctx, TrashPrefix+day+"/"+key, data)
			}
			if err != nil {
				addErr(it.Path, fmt.Errorf("trash copy failed, not deleted: %w", err))
				continue
			}
			keys = append(keys, key)
			newTombs[key] = Tombstone{DeletedAt: time.Now().UTC(), Device: s.device}
		}
		if len(keys) > 0 {
			if err := s.storage.DeleteBatch(ctx, keys); err != nil {
				res.Errors = append(res.Errors, fmt.Errorf("delete batch: %w", err))
			} else {
				for _, it := range deletes {
					if _, ok := newTombs[it.Remote.Key]; ok {
						s.state.RemoveFile(it.Path)
						res.Deleted = append(res.Deleted, it.Path)
					}
				}
			}
		}
	}

	if len(newTombs) > 0 || len(tombsCleared) > 0 {
		tombs := plan.tombstones
		if tombs == nil {
			tombs = map[string]Tombstone{}
		}
		for k := range tombsCleared {
			delete(tombs, k)
		}
		for k, v := range newTombs {
			tombs[k] = v
		}
		if err := s.saveTombstones(ctx, tombs); err != nil {
			res.Errors = append(res.Errors, fmt.Errorf("tombstones: %w", err))
		}
	}

	if len(res.Uploaded) > 0 || len(res.Deleted) > 0 {
		if err := s.uploadManifest(ctx); err != nil {
			res.Errors = append(res.Errors, fmt.Errorf("manifest: %w", err))
		}
	}

	// Rebuild prompt history when sessions or history arrived.
	needHistory := false
	for _, p := range append(append([]string{}, res.Downloaded...), res.Merged...) {
		if p == HistoryFile || strings.HasPrefix(p, "projects/") {
			needHistory = true
			break
		}
	}
	if needHistory {
		hr, err := RebuildHistory(s.claudeDir)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Errorf("rebuild history: %w", err))
		} else {
			res.HistoryRebuilt = true
			res.HistoryAdded = hr.Merged - hr.Existing
			// Upload the rebuilt history so other machines get it too, when
			// it differs from what was last synced.
			h, herr := HashFile(filepath.Join(s.claudeDir, HistoryFile))
			if b := s.state.GetFile(HistoryFile); herr == nil && (b == nil || b.Hash != h) {
				if _, err := s.upload(ctx, HistoryFile); err != nil {
					res.Errors = append(res.Errors, fmt.Errorf("upload rebuilt history: %w", err))
				}
			}
		}
	}

	s.state.Schema = StateSchema
	s.state.LastSync = time.Now()
	if len(res.Uploaded) > 0 {
		s.state.LastPush = time.Now()
	}
	if len(res.Downloaded) > 0 {
		s.state.LastPull = time.Now()
	}
	if err := s.state.Save(); err != nil {
		return res, fmt.Errorf("failed to save state: %w", err)
	}
	sort.Strings(res.Uploaded)
	sort.Strings(res.Downloaded)
	sort.Strings(res.Deleted)
	return res, nil
}

// ---------------------------------------------------------------------------
// Transfers

// encode turns local bytes into the remote object format (v1 compatible):
// path-normalize, gzip, age-encrypt.
func (s *Syncer) encode(relPath string, data []byte) ([]byte, error) {
	data = s.paths.NormalizeContent(relPath, data)
	compressed, err := gzipCompress(data)
	if err != nil {
		return nil, fmt.Errorf("compress: %w", err)
	}
	return s.encryptor.Encrypt(compressed)
}

// decode reverses encode for this machine.
func (s *Syncer) decode(relPath string, enc []byte) ([]byte, error) {
	data, err := s.encryptor.Decrypt(enc)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	if isGzipped(data) {
		if data, err = gzipDecompress(data); err != nil {
			return nil, fmt.Errorf("decompress: %w", err)
		}
	}
	return s.paths.ResolveContent(relPath, data), nil
}

func (s *Syncer) putObject(ctx context.Context, key string, data []byte) (string, error) {
	if u, ok := s.storage.(storage.ETagUploader); ok {
		return u.UploadETag(ctx, key, data)
	}
	if err := s.storage.Upload(ctx, key, data); err != nil {
		return "", err
	}
	info, err := s.storage.Head(ctx, key)
	if err != nil {
		return "", nil // baseline without ETag falls back to the v1 time rule
	}
	return storage.NormalizeETag(info.ETag), nil
}

// upload sends the local file. tooBig is true (and nothing is sent) when the
// encrypted object would exceed rules.MaxUploadBytes.
func (s *Syncer) upload(ctx context.Context, relPath string) (tooBig bool, err error) {
	full := filepath.Join(s.claudeDir, filepath.FromSlash(relPath))
	data, err := os.ReadFile(full)
	if err != nil {
		return false, fmt.Errorf("read: %w", err)
	}
	info, err := os.Stat(full)
	if err != nil {
		return false, err
	}
	hash := hashBytes(data)
	enc, err := s.encode(relPath, data)
	if err != nil {
		return false, err
	}
	if len(enc) > rules.MaxUploadBytes {
		s.state.mu.Lock()
		s.state.TooLarge[relPath] = hash
		s.state.mu.Unlock()
		return true, nil
	}
	etag, err := s.putObject(ctx, s.remoteKey(relPath), enc)
	if err != nil {
		return false, err
	}
	s.state.mu.Lock()
	delete(s.state.TooLarge, relPath)
	s.state.mu.Unlock()
	s.state.SetFile(relPath, hash, info.Size(), info.ModTime(), etag)
	return false, nil
}

// fetch downloads and decodes a remote object.
func (s *Syncer) fetch(ctx context.Context, relPath string, r *storage.ObjectInfo) ([]byte, error) {
	enc, err := s.storage.Download(ctx, r.Key)
	if err != nil {
		return nil, err
	}
	return s.decode(relPath, enc)
}

// writeLocal writes content for relPath atomically, keeping an existing
// file's mode, and records the baseline against etag.
func (s *Syncer) writeLocal(relPath string, data []byte, etag string, mtime *time.Time) error {
	full := filepath.Join(s.claudeDir, filepath.FromSlash(relPath))
	if !strings.HasPrefix(filepath.Clean(full), filepath.Clean(s.claudeDir)+string(filepath.Separator)) {
		return fmt.Errorf("refusing to write outside %s", s.claudeDir)
	}
	if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
		return err
	}
	mode := os.FileMode(0600)
	if rules.Executable(relPath) {
		mode = 0700
	}
	if fi, err := os.Stat(full); err == nil {
		mode = fi.Mode().Perm()
	}
	if err := writeFileAtomic(full, data, mode); err != nil {
		return fmt.Errorf("write (file in use?): %w", err)
	}
	if mtime != nil && !mtime.IsZero() {
		_ = os.Chtimes(full, *mtime, *mtime)
	}
	if etag == "" {
		return nil
	}
	fi, err := os.Stat(full)
	if err != nil {
		return err
	}
	s.state.SetFile(relPath, hashBytes(data), fi.Size(), fi.ModTime(), etag)
	return nil
}

func (s *Syncer) download(ctx context.Context, relPath string, r *storage.ObjectInfo, manifest *FileManifest) error {
	data, err := s.fetch(ctx, relPath, r)
	if err != nil {
		return err
	}
	var mtime *time.Time
	if manifest != nil {
		if m, ok := manifest.Files[relPath]; ok {
			mtime = &m.ModTime
		}
	}
	return s.writeLocal(relPath, data, r.ETag, mtime)
}

// resolve handles both-changed files and first-contact verification.
func (s *Syncer) resolve(ctx context.Context, it Item, manifest *FileManifest, res *Result, mu *sync.Mutex) error {
	remoteData, err := s.fetch(ctx, it.Path, it.Remote)
	if err != nil {
		return err
	}
	full := filepath.Join(s.claudeDir, filepath.FromSlash(it.Path))
	localData, err := os.ReadFile(full)
	if err != nil {
		return err
	}
	if bytes.Equal(localData, remoteData) {
		fi, err := os.Stat(full)
		if err != nil {
			return err
		}
		s.state.SetFile(it.Path, hashBytes(localData), fi.Size(), fi.ModTime(), it.Remote.ETag)
		mu.Lock()
		res.Recorded++
		mu.Unlock()
		return nil
	}

	note := func(list *[]string) {
		mu.Lock()
		*list = append(*list, it.Path)
		mu.Unlock()
	}
	uploadLocal := func() error {
		tooBig, err := s.upload(ctx, it.Path)
		if err == nil && tooBig {
			note(&res.TooLarge)
		}
		return err
	}

	switch rules.ClassOf(it.Path) {
	case rules.History:
		merged := unionLines(localData, remoteData)
		if err := s.writeLocal(it.Path, merged, "", nil); err != nil {
			return err
		}
		note(&res.Merged)
		return uploadLocal()

	case rules.Transcript:
		switch {
		case bytes.HasPrefix(localData, remoteData):
			note(&res.Merged)
			return uploadLocal()
		case bytes.HasPrefix(remoteData, localData):
			note(&res.Merged)
			return s.writeLocal(it.Path, remoteData, it.Remote.ETag, nil)
		}
		// Diverged: keep local, but keep the remote copy on disk (not synced).
		copyPath := conflictCopyName(it.Path)
		if err := s.writeLocal(copyPath, remoteData, "", nil); err != nil {
			return err
		}
		mu.Lock()
		res.Merged = append(res.Merged, it.Path)
		res.Copies[it.Path] = copyPath
		mu.Unlock()
		return uploadLocal()

	case rules.KeepLocal:
		note(&res.Merged)
		return uploadLocal()
	}

	// KeepBoth: write the remote copy beside the local file and freeze.
	copyPath := conflictCopyName(it.Path)
	if err := s.writeLocal(copyPath, remoteData, "", nil); err != nil {
		return err
	}
	base := ""
	if b := s.state.GetFile(it.Path); b != nil {
		base = b.Hash
	}
	fi, _ := os.Stat(full)
	var size int64
	var mtime time.Time
	if fi != nil {
		size, mtime = fi.Size(), fi.ModTime()
	}
	// Baseline: old local hash (so local reads as changed once the copy is
	// deleted) and the remote ETag we have now seen (so remote reads as
	// unchanged). Deleting the copy therefore means "my local file wins".
	s.state.SetFile(it.Path, base, size, mtime, it.Remote.ETag)
	s.state.mu.Lock()
	s.state.Conflicts[it.Path] = &ConflictState{Copy: copyPath, Created: time.Now()}
	s.state.mu.Unlock()
	mu.Lock()
	res.KeptBoth = append(res.KeptBoth, it.Path)
	res.Copies[it.Path] = copyPath
	mu.Unlock()
	return nil
}

func conflictCopyName(relPath string) string {
	return relPath + ".conflict-remote-" + time.Now().Format("20060102-150405")
}

// unionLines merges two JSONL files: every distinct non-empty line from both,
// local order first, then remote-only lines.
func unionLines(local, remote []byte) []byte {
	seen := make(map[string]bool)
	var out bytes.Buffer
	for _, src := range [][]byte{local, remote} {
		for _, line := range bytes.Split(src, []byte("\n")) {
			if len(bytes.TrimSpace(line)) == 0 || seen[string(line)] {
				continue
			}
			seen[string(line)] = true
			out.Write(line)
			out.WriteByte('\n')
		}
	}
	return out.Bytes()
}

// ---------------------------------------------------------------------------
// Remote metadata

func (s *Syncer) loadTombstones(ctx context.Context, keys map[string]storage.ObjectInfo) (map[string]Tombstone, error) {
	tombs := make(map[string]Tombstone)
	if _, ok := keys[TombstonesKey]; !ok {
		return tombs, nil
	}
	enc, err := s.storage.Download(ctx, TombstonesKey)
	if err != nil {
		return nil, fmt.Errorf("read tombstones: %w", err)
	}
	data, err := s.encryptor.Decrypt(enc)
	if err != nil {
		return nil, fmt.Errorf("decrypt tombstones: %w", err)
	}
	if isGzipped(data) {
		if data, err = gzipDecompress(data); err != nil {
			return nil, err
		}
	}
	if err := json.Unmarshal(data, &tombs); err != nil {
		return nil, fmt.Errorf("parse tombstones: %w", err)
	}
	return tombs, nil
}

func (s *Syncer) saveTombstones(ctx context.Context, tombs map[string]Tombstone) error {
	data, err := json.Marshal(tombs)
	if err != nil {
		return err
	}
	gz, err := gzipCompress(data)
	if err != nil {
		return err
	}
	enc, err := s.encryptor.Encrypt(gz)
	if err != nil {
		return err
	}
	return s.storage.Upload(ctx, TombstonesKey, enc)
}

// uploadManifest writes file mtimes in the exact shape v1 reads.
func (s *Syncer) uploadManifest(ctx context.Context) error {
	manifest := FileManifest{Files: make(map[string]FileMetadata)}
	s.state.mu.Lock()
	for p, fs := range s.state.Files {
		manifest.Files[p] = FileMetadata{ModTime: fs.ModTime}
	}
	s.state.mu.Unlock()
	data, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	gz, err := gzipCompress(data)
	if err != nil {
		return err
	}
	enc, err := s.encryptor.Encrypt(gz)
	if err != nil {
		return err
	}
	return s.storage.Upload(ctx, ManifestKey+".age", enc)
}

func (s *Syncer) downloadManifest(ctx context.Context) (*FileManifest, error) {
	enc, err := s.storage.Download(ctx, ManifestKey+".age")
	if err != nil {
		return nil, err
	}
	data, err := s.encryptor.Decrypt(enc)
	if err != nil {
		return nil, err
	}
	if isGzipped(data) {
		if data, err = gzipDecompress(data); err != nil {
			return nil, err
		}
	}
	var m FileManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// VerifyKey proves the local key decrypts the bucket's data. It returns nil
// for an empty bucket. A typo'd passphrase yields a valid but different key;
// syncing with it would make this machine's uploads unreadable elsewhere.
func (s *Syncer) VerifyKey(ctx context.Context) (empty bool, err error) {
	objects, err := s.storage.List(ctx, "")
	if err != nil {
		return false, fmt.Errorf("cannot list bucket: %w", err)
	}
	var probe *storage.ObjectInfo
	for i := range objects {
		o := objects[i]
		if o.Key == ManifestKey+".age" {
			probe = &o
			break
		}
		if strings.HasSuffix(o.Key, ".age") && o.Size < 1<<20 && (probe == nil || o.Size < probe.Size) {
			probe = &o
		}
	}
	if probe == nil {
		return len(objects) == 0, nil
	}
	enc, err := s.storage.Download(ctx, probe.Key)
	if err != nil {
		return false, fmt.Errorf("cannot read bucket: %w", err)
	}
	if _, err := s.encryptor.Decrypt(enc); err != nil {
		return false, fmt.Errorf("this passphrase does not decrypt the existing data in the bucket (wrong passphrase?)")
	}
	return false, nil
}

// RepairWindowsTranscripts fixes JSON lines v1 corrupted on Windows (see
// PathMapper.RepairWindowsJSON). Baselines are moved to the repaired hash so
// the repair is not uploaded as a change. Returns files and lines fixed.
func (s *Syncer) RepairWindowsTranscripts() (files, lines int, err error) {
	if s.paths == nil || !s.paths.windows {
		return 0, 0, nil
	}
	local, err := s.scanLocal()
	if err != nil {
		return 0, 0, err
	}
	valid := func(b []byte) bool { return json.Valid(bytes.TrimRight(b, "\r")) }
	for p, lf := range local {
		if !IsPortableContentPath(p) || !isJSONPath(p) {
			continue
		}
		full := filepath.Join(s.claudeDir, filepath.FromSlash(p))
		data, err := os.ReadFile(full)
		if err != nil {
			continue
		}
		fixed, n := s.paths.RepairWindowsJSON(p, data, valid)
		if n == 0 {
			continue
		}
		if err := writeFileAtomic(full, fixed, lf.Mode); err != nil {
			continue // in use; next migrate run or download will fix it
		}
		files++
		lines += n
		if b := s.state.GetFile(p); b != nil && b.Hash == lf.Hash {
			fi, _ := os.Stat(full)
			if fi != nil {
				s.state.SetFile(p, hashBytes(fixed), fi.Size(), fi.ModTime(), b.RemoteETag)
				s.state.Files[p].Uploaded = b.Uploaded
			}
		}
	}
	return files, lines, s.state.Save()
}

// ---------------------------------------------------------------------------
// Prune: the only way to delete out-of-scope remote objects.

// PrunePlan lists remote objects outside the rules, plus trash older than
// 30 days.
type PrunePlan struct {
	OutOfScope []string
	OldTrash   []string
	Bytes      int64
}

func (s *Syncer) BuildPrune(ctx context.Context) (*PrunePlan, error) {
	objects, err := s.storage.List(ctx, "")
	if err != nil {
		return nil, err
	}
	pp := &PrunePlan{}
	cutoff := time.Now().UTC().AddDate(0, 0, -30).Format("20060102")
	for _, o := range objects {
		switch {
		case strings.HasPrefix(o.Key, TrashPrefix):
			day := strings.SplitN(strings.TrimPrefix(o.Key, TrashPrefix), "/", 2)[0]
			if day < cutoff {
				pp.OldTrash = append(pp.OldTrash, o.Key)
				pp.Bytes += o.Size
			}
		case strings.HasPrefix(o.Key, "_metadata/"), strings.HasPrefix(o.Key, "_external/"):
		case strings.HasSuffix(o.Key, ".age"):
			local, ok := s.paths.ResolveRelPath(strings.TrimSuffix(o.Key, ".age"))
			if ok && !rules.InScope(local) {
				pp.OutOfScope = append(pp.OutOfScope, o.Key)
				pp.Bytes += o.Size
			}
		}
	}
	return pp, nil
}

// ExecutePrune moves out-of-scope objects to trash, then deletes them and the
// old trash.
func (s *Syncer) ExecutePrune(ctx context.Context, pp *PrunePlan) error {
	day := time.Now().UTC().Format("20060102")
	var del []string
	for _, k := range pp.OutOfScope {
		data, err := s.storage.Download(ctx, k)
		if err == nil {
			err = s.storage.Upload(ctx, TrashPrefix+day+"/"+k, data)
		}
		if err != nil {
			s.log("skip %s: %v", k, err)
			continue
		}
		del = append(del, k)
	}
	del = append(del, pp.OldTrash...)
	return s.storage.DeleteBatch(ctx, del)
}

// ---------------------------------------------------------------------------
// gzip helpers

func isGzipped(data []byte) bool {
	return len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b
}

func gzipCompress(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	w, err := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(data); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func gzipDecompress(data []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	limited := io.LimitReader(r, maxDecompressedSize+1)
	result, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(result)) > maxDecompressedSize {
		return nil, fmt.Errorf("decompressed data exceeds %d bytes limit", maxDecompressedSize)
	}
	return result, nil
}
