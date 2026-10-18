package sync

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/tawanorg/claude-sync/internal/config"
)

// StateSchema is the state.json schema written by v2. v1 files have no
// schema field (0). v1 ignores unknown fields, so rollback to v1 still works.
const StateSchema = 2

// FileState is the last-synced baseline for one file. Hash is the sha256 of
// the local file as last synced; RemoteETag is the remote object's ETag at
// that moment. A sync compares the current local hash and the current remote
// ETag (from the bucket listing) against this baseline; no clocks involved.
type FileState struct {
	Path       string    `json:"path"`
	Hash       string    `json:"hash"`
	Size       int64     `json:"size"`
	ModTime    time.Time `json:"mod_time"`
	Uploaded   time.Time `json:"uploaded,omitempty"`
	RemoteETag string    `json:"remote_etag,omitempty"`
}

// ConflictState records a keep-both conflict. While Copy exists on disk the
// path is frozen: nothing is uploaded or downloaded for it.
type ConflictState struct {
	Copy    string    `json:"copy"`
	Created time.Time `json:"created"`
}

type SyncState struct {
	Schema   int                   `json:"schema,omitempty"`
	Files    map[string]*FileState `json:"files"`
	LastSync time.Time             `json:"last_sync"`
	DeviceID string                `json:"device_id"`
	LastPush time.Time             `json:"last_push,omitempty"`
	LastPull time.Time             `json:"last_pull,omitempty"`

	// Conflicts are frozen keep-both paths, keyed by local relative path.
	Conflicts map[string]*ConflictState `json:"conflicts,omitempty"`

	// TooLarge remembers files skipped for size, keyed by path, value = hash,
	// so an unchanged oversized file is not recompressed on every sync.
	TooLarge map[string]string `json:"too_large,omitempty"`

	// MCPBaseline stores the last-synced normalized MCP server configs for three-way merge.
	MCPBaseline json.RawMessage `json:"mcp_baseline,omitempty"`

	savePath string     `json:"-"`
	mu       sync.Mutex `json:"-"`
}

func LoadState() (*SyncState, error) {
	return loadStateFromPath(config.StateFilePath())
}

// LoadStateFromDir loads state from a custom directory (for testing)
func LoadStateFromDir(dir string) (*SyncState, error) {
	statePath := filepath.Join(dir, config.StateFile)
	state, err := loadStateFromPath(statePath)
	if err != nil {
		return nil, err
	}
	state.savePath = statePath
	return state, nil
}

func loadStateFromPath(statePath string) (*SyncState, error) {
	data, err := os.ReadFile(statePath)
	if err != nil {
		if os.IsNotExist(err) {
			s := NewState()
			s.savePath = statePath
			return s, nil
		}
		return nil, fmt.Errorf("failed to read state: %w", err)
	}

	var state SyncState
	if err := json.Unmarshal(data, &state); err != nil {
		// state.json is a cache. A corrupt one is set aside and rebuilt from
		// the bucket listing on the next sync.
		backup := fmt.Sprintf("%s.corrupt-%d", statePath, time.Now().Unix())
		if renameErr := os.Rename(statePath, backup); renameErr == nil {
			fmt.Fprintf(os.Stderr, "Warning: state file was corrupt; moved to %s and rebuilding\n", backup)
		}
		s := NewState()
		s.savePath = statePath
		return s, nil
	}
	if state.Files == nil {
		state.Files = make(map[string]*FileState)
	}
	if state.Conflicts == nil {
		state.Conflicts = make(map[string]*ConflictState)
	}
	if state.TooLarge == nil {
		state.TooLarge = make(map[string]string)
	}
	state.savePath = statePath
	return &state, nil
}

func NewState() *SyncState {
	hostname, _ := os.Hostname()
	return &SyncState{
		Schema:    StateSchema,
		Files:     make(map[string]*FileState),
		Conflicts: make(map[string]*ConflictState),
		TooLarge:  make(map[string]string),
		DeviceID:  hostname,
	}
}

// Save writes state atomically (temp file + rename).
func (s *SyncState) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	statePath := s.savePath
	if statePath == "" {
		statePath = config.StateFilePath()
	}
	dir := filepath.Dir(statePath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("failed to create state directory: %w", err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to serialize state: %w", err)
	}
	return writeFileAtomic(statePath, data, 0600)
}

// SetFile records a new baseline for relPath.
func (s *SyncState) SetFile(relPath, hash string, size int64, mtime time.Time, etag string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Files[relPath] = &FileState{
		Path:       relPath,
		Hash:       hash,
		Size:       size,
		ModTime:    mtime,
		Uploaded:   time.Now(),
		RemoteETag: etag,
	}
}

func (s *SyncState) GetFile(relativePath string) *FileState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Files[relativePath]
}

func (s *SyncState) RemoveFile(relativePath string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.Files, relativePath)
}

// IsEmpty returns true if no files have been synced yet (first sync)
func (s *SyncState) IsEmpty() bool {
	return len(s.Files) == 0 && s.LastSync.IsZero()
}

// GetMCPBaseline returns the last-synced MCP server configs used for three-way merge.
func (s *SyncState) GetMCPBaseline() (MCPServers, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.MCPBaseline) == 0 {
		return nil, nil
	}
	var servers MCPServers
	if err := json.Unmarshal(s.MCPBaseline, &servers); err != nil {
		return nil, fmt.Errorf("failed to parse MCP baseline: %w", err)
	}
	return servers, nil
}

// SetMCPBaseline stores the normalized MCP server configs as the baseline for future merges.
func (s *SyncState) SetMCPBaseline(servers MCPServers) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if servers == nil {
		s.MCPBaseline = nil
		return nil
	}
	data, err := json.Marshal(servers)
	if err != nil {
		return fmt.Errorf("failed to serialize MCP baseline: %w", err)
	}
	s.MCPBaseline = data
	return nil
}

func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// writeFileAtomic writes data to a temp file in the same directory and renames
// it over path. On Windows the rename fails if another process holds the
// target open; the caller reports that and retries next sync.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".claude-sync-tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}
