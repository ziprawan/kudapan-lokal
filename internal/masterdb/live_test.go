package masterdb

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"kudapan_lokal/internal/savedata"
)

func TestLiveResolveAndDownloadMasterDB(t *testing.T) {
	if os.Getenv("UMA_LIVE_TEST") != "1" {
		t.Skip("skipping live test; set UMA_LIVE_TEST=1 to fetch real master.mdb from Uma Musume CDN")
	}

	store, err := savedata.LoadOrInit(filepath.Join(t.TempDir(), "savedata.json"))
	if err != nil {
		t.Fatalf("LoadOrInit() error = %v", err)
	}
	recv, err := New(store, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	url, err := recv.ResolveMasterURL(ctx)
	if err != nil {
		t.Fatalf("ResolveMasterURL() error = %v", err)
	}
	t.Logf("resolved master.mdb.lz4 URL = %s", url)
	t.Logf("updated savedata RES-VER = %s", store.Data().ResVer)

	outPath := filepath.Join(t.TempDir(), "master.mdb")
	info, err := recv.DownloadMaster(ctx, outPath)
	if err != nil {
		t.Fatalf("DownloadMaster() error = %v", err)
	}
	t.Logf("downloaded MasterInfo = %+v", info)

	fi, err := os.Stat(outPath)
	if err != nil {
		t.Fatalf("Stat(%q) error = %v", outPath, err)
	}
	if fi.Size() < 1024 {
		t.Fatalf("downloaded master.mdb unexpectedly small: %d bytes", fi.Size())
	}

	f, err := os.Open(outPath)
	if err != nil {
		t.Fatalf("Open(%q) error = %v", outPath, err)
	}
	defer f.Close()

	hdr := make([]byte, len(sqliteMagic))
	if _, err := f.Read(hdr); err != nil {
		t.Fatalf("Read header error = %v", err)
	}
	if !bytes.Equal(hdr, []byte(sqliteMagic)) {
		t.Fatalf("header = %q, want %q", hdr, sqliteMagic)
	}
	t.Logf("verified SQLite 3 master.mdb (%d bytes)", fi.Size())
}
