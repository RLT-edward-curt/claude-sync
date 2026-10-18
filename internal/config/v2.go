package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// V1RuleKeys are config.yaml keys v2 ignores: sync rules are built into the
// binary (internal/rules).
var V1RuleKeys = []string{"sync_paths", "exclude", "scope", "path_map"}

// HasV1Rules reports whether the config still carries per-machine rules.
func (c *Config) HasV1Rules() bool {
	return len(c.SyncPaths) > 0 || len(c.Exclude) > 0 || c.Scope != "" || len(c.PathMap) > 0
}

// WriteFileAtomic writes a 0600 file via temp + rename.
func WriteFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// BackupFile copies path into <config dir>/backups/<name>.<tag>-<timestamp>
// with mode 0600. The backups dir is outside ~/.claude, so credentials in a
// config backup never travel through sync. Returns the backup path, or ""
// if path does not exist.
func BackupFile(path, tag string) (string, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	dst := filepath.Join(ConfigDirPath(), "backups",
		fmt.Sprintf("%s.%s-%s", filepath.Base(path), tag, time.Now().Format("20060102-150405")))
	return dst, WriteFileAtomic(dst, data)
}

// StripV1Rules removes V1RuleKeys from config.yaml, preserving every other
// key and value. It never prints the file. Returns true if it changed it.
func StripV1Rules() (bool, error) {
	path := ConfigFilePath()
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return false, fmt.Errorf("failed to parse config")
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return false, nil
	}
	m := doc.Content[0]
	changed := false
	var kept []*yaml.Node
	for i := 0; i+1 < len(m.Content); i += 2 {
		k := m.Content[i].Value
		drop := false
		for _, rk := range V1RuleKeys {
			if k == rk {
				drop = true
			}
		}
		if drop {
			changed = true
			continue
		}
		kept = append(kept, m.Content[i], m.Content[i+1])
	}
	if !changed {
		return false, nil
	}
	m.Content = kept
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return false, err
	}
	return true, WriteFileAtomic(path, out)
}
