package watcher

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"kudapan_lokal/internal/masterdb"
	"kudapan_lokal/internal/savedata"
)

type fakeFetcher struct {
	resVer string
	srcDB  string
	calls  int
}

func (f *fakeFetcher) ResolveMaster(context.Context) (*masterdb.MasterInfo, error) {
	return &masterdb.MasterInfo{
		ResourceVersion: f.resVer,
		Platform:        "Windows",
	}, nil
}

func (f *fakeFetcher) DownloadMaster(_ context.Context, dstPath string) (*masterdb.MasterInfo, error) {
	f.calls++
	if err := copyFileAtomic(f.srcDB, dstPath); err != nil {
		return nil, err
	}
	return &masterdb.MasterInfo{ResourceVersion: f.resVer}, nil
}

type fakeNotifier struct {
	messages []string
	failOn   int
}

func (n *fakeNotifier) SendMessage(_ context.Context, text string) error {
	if n.failOn > 0 && len(n.messages)+1 == n.failOn {
		return errors.New("simulated telegram failure")
	}
	n.messages = append(n.messages, text)
	return nil
}

func TestWatcherSeedDiffRetryAndSkip(t *testing.T) {
	dir := t.TempDir()
	store, err := savedata.LoadOrInit(filepath.Join(dir, "savedata.json"))
	if err != nil {
		t.Fatalf("LoadOrInit() error = %v", err)
	}

	seedDB := filepath.Join(dir, "seed.mdb")
	newDB := filepath.Join(dir, "server_v2.mdb")
	now := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)

	createWatcherTestDB(t, seedDB, false)
	createWatcherTestDB(t, newDB, true)

	fetcher := &fakeFetcher{resVer: "10008790", srcDB: newDB}
	notifier := &fakeNotifier{failOn: 2} // Fail on 2nd message first to verify atomic retry safety.

	cfg := Config{
		StatePath:        filepath.Join(dir, "state.json"),
		MasterDBPath:     filepath.Join(dir, "master.mdb"),
		SeedMasterDBPath: seedDB,
		Interval:         time.Hour,
	}
	svc, err := New(cfg, store, fetcher, notifier, WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	// First attempt fails sending message 2 -> LastResVer must NOT advance, baseline remains seedDB.
	if _, err := svc.RunOnce(context.Background()); err == nil {
		t.Fatal("expected error when notifier fails")
	}
	stAfterFail, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatalf("LoadState() after fail error = %v", err)
	}
	if stAfterFail.LastResVer != "" {
		t.Fatalf("expected LastResVer to stay empty after failed notify, got %q", stAfterFail.LastResVer)
	}

	// Second attempt succeeds and sends both summary and campaign_data messages.
	notifier.failOn = 0
	notifier.messages = nil
	res, err := svc.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce() retry error = %v", err)
	}
	if !res.Updated || res.OldResVer != savedata.DefaultResVer || res.NewResVer != "10008790" {
		t.Fatalf("unexpected CheckResult: %+v", res)
	}
	if len(res.SentMessages) != 2 {
		t.Fatalf("sent %d messages, want 2: %v", len(res.SentMessages), res.SentMessages)
	}
	if !strings.Contains(res.SentMessages[0], "campaign_data:\n  1 add(s)") {
		t.Errorf("unexpected summary message:\n%s", res.SentMessages[0])
	}
	if !strings.Contains(res.SentMessages[1], "Winter Campaign") ||
		!strings.Contains(res.SentMessages[1], "Starts: 2026-11-23 05:00:00 UTC (in 2 days 5 hours)") ||
		!strings.Contains(res.SentMessages[1], "Ends: 2026-11-30 05:00:00 UTC (in 9 days 5 hours)") {
		t.Errorf("unexpected campaign message:\n%s", res.SentMessages[1])
	}

	// Third run with same RES-VER skips download and notification.
	prevCalls := fetcher.calls
	notifier.messages = nil
	resSame, err := svc.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce() same version error = %v", err)
	}
	if resSame.Updated || fetcher.calls != prevCalls || len(notifier.messages) != 0 {
		t.Fatalf("expected unchanged run to skip download/notify: res=%+v calls=%d/%d msgs=%d",
			resSame, fetcher.calls, prevCalls, len(notifier.messages))
	}
}

func TestWatcherFirstRunWithoutSeedInitializesBaseline(t *testing.T) {
	dir := t.TempDir()
	store, err := savedata.LoadOrInit(filepath.Join(dir, "savedata.json"))
	if err != nil {
		t.Fatalf("LoadOrInit() error = %v", err)
	}

	srvDB := filepath.Join(dir, "srv.mdb")
	createWatcherTestDB(t, srvDB, true)

	fetcher := &fakeFetcher{resVer: "10008790", srcDB: srvDB}
	notifier := &fakeNotifier{}
	svc, err := New(Config{
		StatePath:    filepath.Join(dir, "state.json"),
		MasterDBPath: filepath.Join(dir, "master.mdb"),
	}, store, fetcher, notifier)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	res, err := svc.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if !res.BaselineInitialized || len(notifier.messages) != 0 {
		t.Fatalf("expected baseline initialization without notifications: %+v msgs=%d", res, len(notifier.messages))
	}
}

func TestWatcherVerboseLoggingAndSchedule(t *testing.T) {
	dir := t.TempDir()
	store, err := savedata.LoadOrInit(filepath.Join(dir, "savedata.json"))
	if err != nil {
		t.Fatalf("LoadOrInit() error = %v", err)
	}

	seedDB := filepath.Join(dir, "seed.mdb")
	newDB := filepath.Join(dir, "server_v2.mdb")
	createWatcherTestDB(t, seedDB, false)
	createWatcherTestDB(t, newDB, true)

	var logBuf bytes.Buffer
	logger := log.New(&logBuf, "", 0)
	now := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)

	fetcher := &fakeFetcher{resVer: "10008790", srcDB: newDB}
	notifier := &fakeNotifier{}

	svc, err := New(Config{
		StatePath:        filepath.Join(dir, "state.json"),
		MasterDBPath:     filepath.Join(dir, "master.mdb"),
		SeedMasterDBPath: seedDB,
		Interval:         time.Hour,
		Verbose:          true,
	}, store, fetcher, notifier, WithLogger(logger), WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	res, err := svc.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	svc.LogCheckResult(res, 150*time.Millisecond)

	logs := logBuf.String()
	for _, want := range []string{
		"[check] starting update check",
		"[check] seeded initial baseline",
		"[check] server resolved RES-VER=10008790",
		"[check] diff complete: 2 table(s) changed, campaign_data (+1 / -0 / ~0)",
		"[check]   table campaign_data: +1 / -0 / ~0",
		"[check] updated RES-VER 10008320 -> 10008790",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs missing %q;\ngot:\n%s", want, logs)
		}
	}
}

func createWatcherTestDB(t *testing.T, path string, withNewCampaign bool) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	defer db.Close()

	stmts := []string{
		`CREATE TABLE campaign_data (campaign_id INTEGER PRIMARY KEY, effect_type_1 INTEGER NOT NULL, start_time INTEGER NOT NULL, end_time INTEGER NOT NULL)`,
		`CREATE TABLE text_data (id INTEGER NOT NULL, category INTEGER NOT NULL, "index" INTEGER NOT NULL, text TEXT NOT NULL, PRIMARY KEY(category, "index"))`,
		`INSERT INTO campaign_data VALUES (1, 2, 1790000000, 1790500000)`,
		`INSERT INTO text_data VALUES (187, 187, 1, 'Existing'), (188, 188, 1, 'Existing desc')`,
	}
	if withNewCampaign {
		start := time.Date(2026, 11, 23, 5, 0, 0, 0, time.UTC).Unix()
		end := time.Date(2026, 11, 30, 5, 0, 0, 0, time.UTC).Unix()
		stmts = append(stmts,
			`INSERT INTO campaign_data VALUES (2, 1, `+itoa(start)+`, `+itoa(end)+`)`,
			`INSERT INTO text_data VALUES (187, 187, 2, 'Winter Campaign'), (188, 188, 2, 'Earn extra points!')`,
		)
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("exec %q: %v", s, err)
		}
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
