// Package watcher implements the periodic RES-VER check, baseline master.mdb
// state persistence, diffing, and Telegram notification dispatch.
package watcher

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// DefaultStatePath is the default path for persisting the last notified RES-VER.
const DefaultStatePath = "data/state.json"

// State tracks the last resource version whose master.mdb diff was processed
// and notified, separate from savedata.json.
type State struct {
	LastResVer       string `json:"last_res_ver"`
	PendingOldResVer string `json:"pending_old_res_ver,omitempty"`
}

// LoadState reads State from path, returning an empty State if the file does not exist.
func LoadState(path string) (State, error) {
	if path == "" {
		path = DefaultStatePath
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return State{}, nil
		}
		return State{}, fmt.Errorf("read watcher state %q: %w", path, err)
	}
	var s State
	if err := json.Unmarshal(raw, &s); err != nil {
		return State{}, fmt.Errorf("parse watcher state %q: %w", path, err)
	}
	return s, nil
}

// SaveStateAtomic persists s atomically to path.
func SaveStateAtomic(path string, s State) error {
	if path == "" {
		path = DefaultStatePath
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create state directory %q: %w", dir, err)
	}
	payload, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal watcher state: %w", err)
	}
	payload = append(payload, '\n')

	tmp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp state file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(payload); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp state file: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp state file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp state file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename state file to %q: %w", path, err)
	}
	cleanup = false
	return nil
}
