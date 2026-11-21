// Package savedata manages persistent Uma Musume client session identity
// and version metadata (APP-VER, RES-VER, ViewerID, and UDID).
package savedata

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/google/uuid"
)

const (
	// DefaultFilePath is the default location for persisted client metadata.
	DefaultFilePath = "savedata.json"
	// DefaultAppVer is the default APP-VER HTTP header value.
	DefaultAppVer = "1.35.3"
	// DefaultResVer is the default RES-VER HTTP header value.
	DefaultResVer = "10008320"
	// DefaultViewerID is the default unauthenticated ViewerID value.
	DefaultViewerID = "0"
)

// Data holds the persisted client metadata fields.
type Data struct {
	AppVer   string `json:"app_ver"`
	ResVer   string `json:"res_ver"`
	ViewerID string `json:"viewer_id"`
	UDID     string `json:"udid"`
}

// Store provides thread-safe access and atomic persistence for Data.
type Store struct {
	mu   sync.RWMutex
	path string
	data Data
}

// LoadOrInit loads savedata from path (or DefaultFilePath when path is empty).
// Missing files or missing fields are initialized with defaults and a new UUIDv4
// UDID, then written atomically to disk.
func LoadOrInit(path string) (*Store, error) {
	if path == "" {
		path = DefaultFilePath
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read savedata %q: %w", path, err)
		}

		var d Data
		if _, err := normalizeAndValidate(&d); err != nil {
			return nil, err
		}
		if err := writeJSONAtomic(path, d); err != nil {
			return nil, err
		}
		return &Store{path: path, data: d}, nil
	}

	var d Data
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("parse savedata %q: %w", path, err)
	}

	changed, err := normalizeAndValidate(&d)
	if err != nil {
		return nil, fmt.Errorf("validate savedata %q: %w", path, err)
	}
	if changed {
		if err := writeJSONAtomic(path, d); err != nil {
			return nil, err
		}
	}

	return &Store{path: path, data: d}, nil
}

// Path returns the backing file path of the store.
func (s *Store) Path() string {
	return s.path
}

// Data returns a snapshot of the stored metadata.
func (s *Store) Data() Data {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data
}

// Save writes the current store state atomically to disk.
func (s *Store) Save() error {
	s.mu.RLock()
	d := s.data
	s.mu.RUnlock()

	if _, err := normalizeAndValidate(&d); err != nil {
		return fmt.Errorf("validate savedata: %w", err)
	}
	return writeJSONAtomic(s.path, d)
}

// Update mutates a copy of the stored data, validates it, persists it
// atomically to disk, and commits the update in memory only on success.
func (s *Store) Update(fn func(*Data) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	next := s.data
	if err := fn(&next); err != nil {
		return err
	}
	if _, err := normalizeAndValidate(&next); err != nil {
		return fmt.Errorf("validate updated savedata: %w", err)
	}
	if err := writeJSONAtomic(s.path, next); err != nil {
		return err
	}
	s.data = next
	return nil
}

func normalizeAndValidate(d *Data) (bool, error) {
	changed := false
	if d.AppVer == "" {
		d.AppVer = DefaultAppVer
		changed = true
	}
	if d.ResVer == "" {
		d.ResVer = DefaultResVer
		changed = true
	}
	if d.ViewerID == "" {
		d.ViewerID = DefaultViewerID
		changed = true
	}
	if d.UDID == "" {
		u, err := uuid.NewRandom()
		if err != nil {
			return false, fmt.Errorf("generate uuidv4 udid: %w", err)
		}
		d.UDID = u.String()
		changed = true
		return changed, nil
	}

	parsed, err := uuid.Parse(d.UDID)
	if err != nil {
		return false, fmt.Errorf("invalid udid %q: %w", d.UDID, err)
	}
	canonical := parsed.String()
	if canonical != d.UDID {
		d.UDID = canonical
		changed = true
	}
	return changed, nil
}

func writeJSONAtomic(path string, d Data) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create savedata directory %q: %w", dir, err)
	}

	payload, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal savedata: %w", err)
	}
	payload = append(payload, '\n')

	tmp, err := os.CreateTemp(dir, ".savedata-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp savedata file: %w", err)
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
		return fmt.Errorf("write temp savedata file: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp savedata file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp savedata file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename savedata file: %w", err)
	}
	cleanup = false
	return nil
}
