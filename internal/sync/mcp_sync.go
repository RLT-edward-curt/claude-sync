package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/tawanorg/claude-sync/internal/config"
)

// claudeJSONPath returns the path to ~/.claude.json, respecting test overrides.
func (s *Syncer) claudeJSONPath() string {
	if s.cfg.ClaudeJSONOverride != "" {
		return s.cfg.ClaudeJSONOverride
	}
	return config.ClaudeJSONPath()
}

// PushMCP reads local MCP server configs, normalizes paths, and uploads them.
func (s *Syncer) PushMCP(ctx context.Context) (*MCPPushResult, error) {
	result := &MCPPushResult{}

	servers, err := ReadMCPServers(s.claudeJSONPath())
	if err != nil {
		return nil, fmt.Errorf("failed to read MCP servers: %w", err)
	}
	if len(servers) == 0 {
		result.Unchanged = true
		return result, nil
	}

	homeDir, _ := os.UserHomeDir()
	normalized, err := NormalizeMCPServers(servers, homeDir)
	if err != nil {
		return nil, fmt.Errorf("failed to normalize MCP paths: %w", err)
	}

	newHash, err := HashMCPServers(normalized)
	if err != nil {
		return nil, fmt.Errorf("failed to hash MCP servers: %w", err)
	}
	if stateFile := s.state.GetFile(config.MCPRemoteKey); stateFile != nil && stateFile.Hash == newHash {
		result.Unchanged = true
		return result, nil
	}

	data, err := json.Marshal(normalized)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize MCP servers: %w", err)
	}
	compressed, err := gzipCompress(data)
	if err != nil {
		return nil, fmt.Errorf("failed to compress: %w", err)
	}
	encrypted, err := s.encryptor.Encrypt(compressed)
	if err != nil {
		return nil, fmt.Errorf("failed to encrypt: %w", err)
	}
	if err := s.storage.Upload(ctx, config.MCPRemoteKey+".age", encrypted); err != nil {
		return nil, fmt.Errorf("failed to upload MCP servers: %w", err)
	}

	s.state.mu.Lock()
	s.state.Files[config.MCPRemoteKey] = &FileState{
		Path: config.MCPRemoteKey, Hash: newHash, Size: int64(len(data)),
		ModTime: time.Now(), Uploaded: time.Now(),
	}
	s.state.mu.Unlock()

	if err := s.state.SetMCPBaseline(normalized); err != nil {
		return nil, fmt.Errorf("failed to save MCP baseline: %w", err)
	}
	if err := s.state.Save(); err != nil {
		return nil, fmt.Errorf("failed to save state: %w", err)
	}
	result.ServersPushed = len(normalized)
	return result, nil
}

// PullMCP downloads remote MCP server configs and merges them with local configs.
func (s *Syncer) PullMCP(ctx context.Context) (*MCPPullResult, error) {
	result := &MCPPullResult{}

	encrypted, err := s.storage.Download(ctx, config.MCPRemoteKey+".age")
	if err != nil {
		result.NoRemote = true
		return result, nil
	}
	decrypted, err := s.encryptor.Decrypt(encrypted)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt MCP data: %w", err)
	}
	if isGzipped(decrypted) {
		if decrypted, err = gzipDecompress(decrypted); err != nil {
			return nil, fmt.Errorf("failed to decompress MCP data: %w", err)
		}
	}
	var remoteServers MCPServers
	if err := json.Unmarshal(decrypted, &remoteServers); err != nil {
		return nil, fmt.Errorf("failed to parse remote MCP servers: %w", err)
	}

	claudeJSON := s.claudeJSONPath()
	localServers, err := ReadMCPServers(claudeJSON)
	if err != nil {
		return nil, fmt.Errorf("failed to read local MCP servers: %w", err)
	}
	if localServers == nil {
		localServers = make(MCPServers)
	}
	homeDir, _ := os.UserHomeDir()
	localNormalized, err := NormalizeMCPServers(localServers, homeDir)
	if err != nil {
		return nil, fmt.Errorf("failed to normalize local MCP paths: %w", err)
	}
	baseline, err := s.state.GetMCPBaseline()
	if err != nil {
		return nil, fmt.Errorf("failed to load MCP baseline: %w", err)
	}
	if baseline == nil {
		baseline = make(MCPServers)
	}

	mergeResult := MergeMCPServers(localNormalized, remoteServers, baseline)
	resolved, err := ResolveMCPServers(mergeResult.Merged, homeDir)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve MCP paths: %w", err)
	}
	if err := WriteMCPServers(claudeJSON, resolved); err != nil {
		return nil, fmt.Errorf("failed to write MCP servers: %w", err)
	}
	if err := s.state.SetMCPBaseline(mergeResult.Merged); err != nil {
		return nil, fmt.Errorf("failed to save MCP baseline: %w", err)
	}
	newHash, _ := HashMCPServers(mergeResult.Merged)
	s.state.mu.Lock()
	s.state.Files[config.MCPRemoteKey] = &FileState{
		Path: config.MCPRemoteKey, Hash: newHash, Size: int64(len(decrypted)),
		ModTime: time.Now(), Uploaded: time.Now(),
	}
	s.state.mu.Unlock()
	if err := s.state.Save(); err != nil {
		return nil, fmt.Errorf("failed to save state: %w", err)
	}

	result.Added = mergeResult.Added
	result.Updated = mergeResult.Updated
	result.Kept = mergeResult.Kept
	result.Conflicts = mergeResult.Conflicts
	return result, nil
}

// MCPStatusResult is the state of local MCP servers compared to the last sync.
type MCPStatusResult struct {
	Servers     MCPServers
	HasChanges  bool
	ServerCount int
}

func (s *Syncer) MCPStatus(ctx context.Context) (*MCPStatusResult, error) {
	servers, err := ReadMCPServers(s.claudeJSONPath())
	if err != nil {
		return nil, fmt.Errorf("failed to read MCP servers: %w", err)
	}
	result := &MCPStatusResult{Servers: servers, ServerCount: len(servers)}
	if servers == nil {
		return result, nil
	}
	homeDir, _ := os.UserHomeDir()
	normalized, err := NormalizeMCPServers(servers, homeDir)
	if err != nil {
		return nil, err
	}
	newHash, err := HashMCPServers(normalized)
	if err != nil {
		return nil, err
	}
	stateFile := s.state.GetFile(config.MCPRemoteKey)
	result.HasChanges = stateFile == nil || stateFile.Hash != newHash
	return result, nil
}
