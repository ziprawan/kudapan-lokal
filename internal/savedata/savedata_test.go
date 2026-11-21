package savedata

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

func TestLoadOrInitCreatesDefaultsAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "savedata.json")

	store, err := LoadOrInit(path)
	if err != nil {
		t.Fatalf("LoadOrInit() error = %v", err)
	}

	d := store.Data()
	if d.AppVer != DefaultAppVer {
		t.Errorf("AppVer = %q, want %q", d.AppVer, DefaultAppVer)
	}
	if d.ResVer != DefaultResVer {
		t.Errorf("ResVer = %q, want %q", d.ResVer, DefaultResVer)
	}
	if d.ViewerID != DefaultViewerID {
		t.Errorf("ViewerID = %q, want %q", d.ViewerID, DefaultViewerID)
	}

	parsed, err := uuid.Parse(d.UDID)
	if err != nil {
		t.Fatalf("uuid.Parse(%q) error = %v", d.UDID, err)
	}
	if parsed.Version() != 4 {
		t.Errorf("UDID version = %d, want 4", parsed.Version())
	}

	reloaded, err := LoadOrInit(path)
	if err != nil {
		t.Fatalf("reload LoadOrInit() error = %v", err)
	}
	if reloaded.Data() != d {
		t.Errorf("reloaded data = %+v, want %+v", reloaded.Data(), d)
	}
}

func TestLoadOrInitBackfillsMissingFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "savedata.json")
	if err := os.WriteFile(path, []byte(`{"app_ver":"1.36.0","viewer_id":"123456"}`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	store, err := LoadOrInit(path)
	if err != nil {
		t.Fatalf("LoadOrInit() error = %v", err)
	}

	d := store.Data()
	if d.AppVer != "1.36.0" {
		t.Errorf("AppVer = %q, want 1.36.0", d.AppVer)
	}
	if d.ViewerID != "123456" {
		t.Errorf("ViewerID = %q, want 123456", d.ViewerID)
	}
	if d.ResVer != DefaultResVer {
		t.Errorf("ResVer = %q, want %q", d.ResVer, DefaultResVer)
	}
	if _, err := uuid.Parse(d.UDID); err != nil {
		t.Errorf("UDID = %q is not a valid UUID: %v", d.UDID, err)
	}
}

func TestLoadOrInitErrorsOnCorruptOrInvalidUDID(t *testing.T) {
	corruptPath := filepath.Join(t.TempDir(), "corrupt.json")
	if err := os.WriteFile(corruptPath, []byte(`{not json`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if _, err := LoadOrInit(corruptPath); err == nil {
		t.Error("expected error for corrupt JSON, got nil")
	}

	badUDIDPath := filepath.Join(t.TempDir(), "bad_udid.json")
	if err := os.WriteFile(badUDIDPath, []byte(`{"udid":"not-a-uuid"}`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if _, err := LoadOrInit(badUDIDPath); err == nil {
		t.Error("expected error for invalid UDID, got nil")
	}
}

func TestStoreUpdateAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "savedata.json")
	store, err := LoadOrInit(path)
	if err != nil {
		t.Fatalf("LoadOrInit() error = %v", err)
	}
	orig := store.Data()

	if err := store.Update(func(d *Data) error {
		d.ResVer = "10009999"
		d.ViewerID = "987654321"
		return nil
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	reloaded, err := LoadOrInit(path)
	if err != nil {
		t.Fatalf("LoadOrInit() after update error = %v", err)
	}
	if got := reloaded.Data(); got.ResVer != "10009999" || got.ViewerID != "987654321" || got.UDID != orig.UDID {
		t.Errorf("updated data = %+v, want ResVer=10009999 ViewerID=987654321 UDID=%s", got, orig.UDID)
	}

	if err := store.Update(func(d *Data) error {
		d.UDID = "invalid-uuid"
		return nil
	}); err == nil {
		t.Fatal("expected Update() to fail on invalid UDID")
	}
	if got := store.Data().UDID; got != orig.UDID {
		t.Errorf("UDID mutated on failed Update(): got %q, want %q", got, orig.UDID)
	}
}
