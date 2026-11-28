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
	"kudapan_lokal/internal/mdbdiff"
	"kudapan_lokal/internal/savedata"
)

type fakeFetcher struct {
	resVer        string
	srcDB         string
	versionCalls  int
	manifestCalls int
	downloadCalls int
}

func (f *fakeFetcher) CheckResourceVersion(context.Context) (*masterdb.ResourceVersionInfo, error) {
	f.versionCalls++
	return &masterdb.ResourceVersionInfo{
		ResourceVersion: f.resVer,
	}, nil
}

func (f *fakeFetcher) ResolveManifest(_ context.Context, rv *masterdb.ResourceVersionInfo) (*masterdb.MasterInfo, error) {
	f.manifestCalls++
	return &masterdb.MasterInfo{
		ResourceVersion: rv.ResourceVersion,
		Platform:        "Windows",
	}, nil
}

func (f *fakeFetcher) DownloadMasterInfo(_ context.Context, _ *masterdb.MasterInfo, dstPath string) error {
	f.downloadCalls++
	return copyFileAtomic(f.srcDB, dstPath)
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

	// Third run with same RES-VER skips manifest fetches, download, and notification.
	prevVersionCalls := fetcher.versionCalls
	prevManifestCalls := fetcher.manifestCalls
	prevDownloadCalls := fetcher.downloadCalls
	notifier.messages = nil
	resSame, err := svc.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce() same version error = %v", err)
	}
	if resSame.Updated ||
		fetcher.versionCalls != prevVersionCalls+1 ||
		fetcher.manifestCalls != prevManifestCalls ||
		fetcher.downloadCalls != prevDownloadCalls ||
		len(notifier.messages) != 0 {
		t.Fatalf("expected unchanged run to check RES-VER once and skip manifests/download/notify: res=%+v ver=%d/%d manifest=%d/%d dl=%d/%d msgs=%d",
			resSame, fetcher.versionCalls, prevVersionCalls+1, fetcher.manifestCalls, prevManifestCalls, fetcher.downloadCalls, prevDownloadCalls, len(notifier.messages))
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
	if fetcher.versionCalls != 1 || fetcher.manifestCalls != 1 || fetcher.downloadCalls != 1 {
		t.Fatalf("unexpected call counts after baseline init: %+v", fetcher)
	}

	// Subsequent check at matching RES-VER skips manifests and download completely.
	resSame, err := svc.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce() second error = %v", err)
	}
	if resSame.Updated || fetcher.versionCalls != 2 || fetcher.manifestCalls != 1 || fetcher.downloadCalls != 1 {
		t.Fatalf("expected unchanged run to skip manifests/download: res=%+v fetcher=%+v", resSame, fetcher)
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

	// Next check at the same RES-VER logs skip of manifest/database fetches.
	if _, err := svc.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() same RES-VER error = %v", err)
	}

	logs := logBuf.String()
	for _, want := range []string{
		"[check] starting update check",
		"[check] seeded initial baseline",
		"[check] server returned RES-VER=10008790",
		"[check] resolving manifest chain for RES-VER=10008790",
		"[check] diff complete: 2 table(s) changed, campaign_data (+1 / -0 / ~0)",
		"[check]   table campaign_data: +1 / -0 / ~0",
		"[check] updated RES-VER 10008320 -> 10008790",
		"[check] RES-VER=10008790 matches local baseline; skipping manifest and database fetches",
		"[check] no single_mode_scenario/champions_schedule changes; skipping notification",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs missing %q;\ngot:\n%s", want, logs)
		}
	}

	// Verify Run() logs the minute-02 aligned next schedule before arming the timer.
	var runLogBuf bytes.Buffer
	schedTime := time.Date(2026, 11, 21, 14, 50, 12, 0, time.UTC)
	schedSvc, err := New(Config{
		StatePath:    filepath.Join(dir, "state.json"),
		MasterDBPath: filepath.Join(dir, "master.mdb"),
		Interval:     time.Hour,
		CheckOnStart: false,
	}, store, fetcher, notifier, WithLogger(log.New(&runLogBuf, "", 0)), WithClock(func() time.Time { return schedTime }))
	if err != nil {
		t.Fatalf("New(schedSvc) error = %v", err)
	}
	runCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := schedSvc.Run(runCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("schedSvc.Run() error = %v, want context.DeadlineExceeded", err)
	}
	wantSched := "next check scheduled at 2026-11-21 15:02:00 UTC (in 11m48s)"
	if !strings.Contains(runLogBuf.String(), wantSched) {
		t.Errorf("Run() logs missing %q;\ngot:\n%s", wantSched, runLogBuf.String())
	}
}

func TestNextAlignedTime(t *testing.T) {
	wib := time.FixedZone("WIB", 7*3600)
	cases := []struct {
		name     string
		now      time.Time
		interval time.Duration
		want     time.Time
		wantWait time.Duration
	}{
		{
			name:     "mid hour +07:00",
			now:      time.Date(2026, 11, 22, 14, 50, 12, 0, wib),
			interval: time.Hour,
			want:     time.Date(2026, 11, 22, 15, 2, 0, 0, wib),
			wantWait: 11*time.Minute + 48*time.Second,
		},
		{
			name:     "top of hour before offset +07:00",
			now:      time.Date(2026, 11, 22, 15, 0, 0, 0, wib),
			interval: time.Hour,
			want:     time.Date(2026, 11, 22, 15, 2, 0, 0, wib),
			wantWait: 2 * time.Minute,
		},
		{
			name:     "exact aligned target +07:00",
			now:      time.Date(2026, 11, 22, 15, 2, 0, 0, wib),
			interval: time.Hour,
			want:     time.Date(2026, 11, 22, 16, 2, 0, 0, wib),
			wantWait: time.Hour,
		},
		{
			name:     "midnight rollover +07:00",
			now:      time.Date(2026, 11, 22, 23, 59, 59, 0, wib),
			interval: time.Hour,
			want:     time.Date(2026, 11, 23, 0, 2, 0, 0, wib),
			wantWait: 2*time.Minute + time.Second,
		},
		{
			name:     "sub-hour interval",
			now:      time.Date(2026, 11, 22, 14, 7, 30, 0, time.UTC),
			interval: 15 * time.Minute,
			want:     time.Date(2026, 11, 22, 14, 15, 0, 0, time.UTC),
			wantWait: 7*time.Minute + 30*time.Second,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NextAlignedTime(tc.now, tc.interval)
			if !got.Equal(tc.want) {
				t.Errorf("NextAlignedTime(%v, %s) = %v, want %v", tc.now, tc.interval, got, tc.want)
			}
			if wait := got.Sub(tc.now); wait != tc.wantWait {
				t.Errorf("wait = %s, want %s", wait, tc.wantWait)
			}
		})
	}
}

func TestWatcherThirdMessageForScenarioAndChampions(t *testing.T) {
	dir := t.TempDir()
	store, err := savedata.LoadOrInit(filepath.Join(dir, "savedata.json"))
	if err != nil {
		t.Fatalf("LoadOrInit() error = %v", err)
	}

	seedDB := filepath.Join(dir, "seed.mdb")
	newDB := filepath.Join(dir, "server_v2.mdb")
	now := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)

	createWatcherTestDB(t, seedDB, false)
	createWatcherTestDB(t, newDB, true)
	addScheduleDiffTestTables(t, seedDB, false)
	addScheduleDiffTestTables(t, newDB, true)

	fetcher := &fakeFetcher{resVer: "10008800", srcDB: newDB}
	notifier := &fakeNotifier{}
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

	res, err := svc.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if len(res.SentMessages) != 3 {
		t.Fatalf("sent %d messages, want 3: %v", len(res.SentMessages), res.SentMessages)
	}
	msg3 := res.SentMessages[2]
	idxScenario := strings.Index(msg3, "single_mode_scenario changes")
	idxChampions := strings.Index(msg3, "champions_schedule changes")
	idxStory := strings.Index(msg3, "story_event_data changes")
	if idxScenario < 0 || idxChampions <= idxScenario || idxStory <= idxChampions ||
		!strings.Contains(msg3, "Grandmasters") ||
		!strings.Contains(msg3, "Pisces Cup") ||
		!strings.Contains(msg3, "Leap into a New World!") {
		t.Errorf("unexpected third message:\n%s", msg3)
	}
}

func TestWatcherSecondMessageIncludesAnnounceData(t *testing.T) {
	dir := t.TempDir()
	store, err := savedata.LoadOrInit(filepath.Join(dir, "savedata.json"))
	if err != nil {
		t.Fatalf("LoadOrInit() error = %v", err)
	}

	seedDB := filepath.Join(dir, "seed.mdb")
	newDB := filepath.Join(dir, "server_v2.mdb")
	now := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)

	createWatcherTestDB(t, seedDB, false)
	createWatcherTestDB(t, newDB, true)
	addAnnounceDiffTestTables(t, seedDB, false, now)
	addAnnounceDiffTestTables(t, newDB, true, now)

	fetcher := &fakeFetcher{resVer: "10008810", srcDB: newDB}
	notifier := &fakeNotifier{}
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

	res, err := svc.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if len(res.SentMessages) != 2 {
		t.Fatalf("sent %d messages, want 2: %v", len(res.SentMessages), res.SentMessages)
	}
	msg2 := res.SentMessages[1]
	for _, want := range []string{
		"campaign_data changes",
		"Winter Campaign",
		"announce_data changes",
		"[Beyond the Horizon] Tokai Teio",
		"Type: Character",
		"[Fire at My Heels] Kitasan Black",
		"Type: Support Card",
	} {
	if !strings.Contains(msg2, want) {
			t.Errorf("second message missing %q:\n%s", want, msg2)
		}
	}
}

func TestWatcherOnUpdateCallback(t *testing.T) {
	dir := t.TempDir()
	store, err := savedata.LoadOrInit(filepath.Join(dir, "savedata.json"))
	if err != nil {
		t.Fatalf("LoadOrInit() error = %v", err)
	}

	seedDB := filepath.Join(dir, "seed.mdb")
	newDB := filepath.Join(dir, "server_v2.mdb")
	now := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)

	createWatcherTestDB(t, seedDB, false)
	createWatcherTestDB(t, newDB, true)

	fetcher := &fakeFetcher{resVer: "10008900", srcDB: newDB}
	notifier := &fakeNotifier{}
	var callbackCalls int
	var gotNewVer string

	svc, err := New(Config{
		StatePath:        filepath.Join(dir, "state.json"),
		MasterDBPath:     filepath.Join(dir, "master.mdb"),
		SeedMasterDBPath: seedDB,
		Interval:         time.Hour,
	}, store, fetcher, notifier,
		WithClock(func() time.Time { return now }),
		WithOnUpdate(func(_ context.Context, _, newResVer string, diff *mdbdiff.DiffResult, _ time.Time) {
			callbackCalls++
			gotNewVer = newResVer
			if diff == nil || len(diff.Campaign.Added) != 1 {
				t.Errorf("unexpected callback diff: %+v", diff)
			}
		}),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if _, err := svc.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce(update) error = %v", err)
	}
	if callbackCalls != 1 || gotNewVer != "10008900" {
		t.Fatalf("callbackCalls=%d gotNewVer=%q, want 1 and 10008900", callbackCalls, gotNewVer)
	}

	// Unchanged RES-VER must not re-invoke callback.
	if _, err := svc.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce(unchanged) error = %v", err)
	}
	if callbackCalls != 1 {
		t.Fatalf("callbackCalls after unchanged check = %d, want 1", callbackCalls)
	}
}

func addAnnounceDiffTestTables(t *testing.T, path string, isNew bool, now time.Time) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	defer db.Close()

	stmts := []string{
		`CREATE TABLE announce_data (id INTEGER PRIMARY KEY, announce_type INTEGER NOT NULL, announce_id INTEGER NOT NULL, priority INTEGER NOT NULL, start_date INTEGER NOT NULL, end_date INTEGER NOT NULL)`,
		`CREATE TABLE announce_character (id INTEGER PRIMARY KEY, movie_id INTEGER NOT NULL, can_end_second INTEGER NOT NULL)`,
		`CREATE TABLE announce_support_card (id INTEGER PRIMARY KEY, type INTEGER NOT NULL, cuts_pattern INTEGER NOT NULL, support_card_id_a INTEGER NOT NULL, support_card_id_b INTEGER NOT NULL, support_card_id_c INTEGER NOT NULL)`,
		`CREATE TABLE gacha_available (gacha_id INTEGER NOT NULL, card_id INTEGER NOT NULL, card_type INTEGER NOT NULL, rarity INTEGER NOT NULL, odds INTEGER NOT NULL, is_pickup INTEGER NOT NULL, recommend_order INTEGER NOT NULL, is_prize_selectable INTEGER NOT NULL, PRIMARY KEY(gacha_id, card_id))`,
	}
	if isNew {
		start := now.Add(24 * time.Hour).Unix()
		end := now.Add(10 * 24 * time.Hour).Unix()
		stmts = append(stmts,
			`INSERT INTO text_data VALUES (4, 4, 100302, '[Beyond the Horizon] Tokai Teio'), (75, 75, 30028, '[Fire at My Heels] Kitasan Black')`,
			`INSERT INTO announce_character VALUES (104, 30010, 11800)`,
			`INSERT INTO announce_support_card VALUES (504, 3, 2, 30028, 0, 0)`,
			`INSERT INTO gacha_available VALUES (30010, 100302, 1, 3, 75000, 1, 1, 0)`,
			`INSERT INTO announce_data VALUES (30010, 1, 104, 1, `+itoa(start)+`, `+itoa(end)+`), (30011, 2, 504, 1, `+itoa(start)+`, `+itoa(end)+`)`,
		)
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("exec %q: %v", s, err)
		}
	}
}

func addScheduleDiffTestTables(t *testing.T, path string, isNew bool) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	defer db.Close()

	stmts := []string{
		`CREATE TABLE single_mode_scenario (id INTEGER PRIMARY KEY, start_date INTEGER NOT NULL, end_date INTEGER NOT NULL, max_speed INTEGER NOT NULL, max_stamina INTEGER NOT NULL, max_pow INTEGER NOT NULL, max_guts INTEGER NOT NULL, max_wiz INTEGER NOT NULL)`,
		`CREATE TABLE champions_schedule (id INTEGER PRIMARY KEY, resource_id INTEGER NOT NULL, start_date INTEGER NOT NULL, end_date INTEGER NOT NULL)`,
		`CREATE TABLE story_event_data (story_event_id INTEGER PRIMARY KEY, announce_id INTEGER NOT NULL, consume_tp_ratio INTEGER NOT NULL, bonus_reward_ratio INTEGER NOT NULL, notice_date INTEGER NOT NULL, start_date INTEGER NOT NULL, middle_date_01 INTEGER NOT NULL, middle_date_02 INTEGER NOT NULL, ending_date INTEGER NOT NULL, end_date INTEGER NOT NULL)`,
		`INSERT INTO single_mode_scenario VALUES (1, 1700000000, 1800000000, 200, 200, 200, 200, 200)`,
		`INSERT INTO champions_schedule VALUES (22, 2, 1794952800, 1795816799)`,
		`INSERT INTO story_event_data VALUES (1022, 22, 100, 100, 1793916000, 1794261600, 1794600000, 1795000000, 1795471199, 1795730399)`,
	}
	if isNew {
		start := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC).Unix()
		end := time.Date(2026, 12, 2, 0, 0, 0, 0, time.UTC).Unix()
		stmts = append(stmts,
			`INSERT INTO text_data VALUES (119, 119, 5, 'Grandmasters'), (206, 206, 23, 'Pisces Cup'), (189, 189, 1023, 'Leap into a New World!')`,
			`INSERT INTO single_mode_scenario VALUES (5, `+itoa(start)+`, 2524608000, 300, 300, 300, 300, 400)`,
			`INSERT INTO champions_schedule VALUES (23, 3, `+itoa(start)+`, `+itoa(end)+`)`,
			`INSERT INTO story_event_data VALUES (1023, 23, 100, 100, `+itoa(start)+`, `+itoa(start)+`, `+itoa(start)+`, `+itoa(start)+`, `+itoa(end)+`, `+itoa(end)+`)`,
		)
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("exec %q: %v", s, err)
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
