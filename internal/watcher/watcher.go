package watcher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"kudapan_lokal/internal/masterdb"
	"kudapan_lokal/internal/mdbdiff"
	"kudapan_lokal/internal/notify"
	"kudapan_lokal/internal/savedata"
)

const (
	// DefaultMasterDBPath is the default cached baseline master.mdb location.
	DefaultMasterDBPath = "data/master.mdb"
	// DefaultInterval is the default polling interval (once per hour).
	DefaultInterval = time.Hour
)

// MasterFetcher abstracts masterdb.Receiver for testing.
type MasterFetcher interface {
	CheckResourceVersion(ctx context.Context) (*masterdb.ResourceVersionInfo, error)
	ResolveManifest(ctx context.Context, rv *masterdb.ResourceVersionInfo) (*masterdb.MasterInfo, error)
	DownloadMasterInfo(ctx context.Context, info *masterdb.MasterInfo, dstPath string) error
}

// Config configures the hourly resource watcher.
type Config struct {
	StatePath        string
	MasterDBPath     string
	SeedMasterDBPath string
	Interval         time.Duration
	CheckOnStart     bool
	Verbose          bool
}

// CheckResult describes the outcome of a single watcher check cycle.
type CheckResult struct {
	CheckedAt           time.Time
	OldResVer           string
	NewResVer           string
	BaselineInitialized bool
	Updated             bool
	Diff                *mdbdiff.DiffResult
	SentMessages        []string
}

// Service coordinates periodic RES-VER checks, master.mdb diffing, and notifications.
type Service struct {
	cfg      Config
	store    *savedata.Store
	fetcher  MasterFetcher
	notifier notify.Notifier
	logger   *log.Logger
	now      func() time.Time
}

// Option customizes a Service instance.
type Option func(*Service)

// WithLogger sets the logger used during Run loop iterations.
func WithLogger(l *log.Logger) Option {
	return func(s *Service) {
		if l != nil {
			s.logger = l
		}
	}
}

// WithClock overrides the clock used for campaign countdown calculations.
func WithClock(now func() time.Time) Option {
	return func(s *Service) {
		if now != nil {
			s.now = now
		}
	}
}

// New constructs a watcher Service.
func New(cfg Config, store *savedata.Store, fetcher MasterFetcher, notifier notify.Notifier, opts ...Option) (*Service, error) {
	if store == nil {
		return nil, errors.New("savedata store cannot be nil")
	}
	if fetcher == nil {
		return nil, errors.New("masterdb fetcher cannot be nil")
	}
	if notifier == nil {
		return nil, errors.New("notifier cannot be nil")
	}
	if cfg.StatePath == "" {
		cfg.StatePath = DefaultStatePath
	}
	if cfg.MasterDBPath == "" {
		cfg.MasterDBPath = DefaultMasterDBPath
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}

	s := &Service{
		cfg:      cfg,
		store:    store,
		fetcher:  fetcher,
		notifier: notifier,
		logger:   log.Default(),
		now:      time.Now,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	return s, nil
}

// Run runs the check loop on cfg.Interval until ctx is canceled.
func (s *Service) Run(ctx context.Context) error {
	s.logger.Printf("watcher loop started (interval=%s, check_on_start=%v, db=%q, state=%q)",
		s.cfg.Interval, s.cfg.CheckOnStart, s.cfg.MasterDBPath, s.cfg.StatePath)

	logNext := func() {
		nextAt := s.now().Add(s.cfg.Interval)
		s.logger.Printf("next check scheduled at %s (in %s)",
			nextAt.Format("2006-01-02 15:04:05 MST"), s.cfg.Interval)
	}

	runCheck := func() {
		started := s.now()
		res, err := s.RunOnce(ctx)
		elapsed := s.now().Sub(started).Round(time.Millisecond)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.logger.Printf("[check] failed (after %s): %v", elapsed, err)
			logNext()
			return
		}
		s.LogCheckResult(res, elapsed)
		logNext()
	}

	if s.cfg.CheckOnStart {
		runCheck()
	} else {
		logNext()
	}

	ticker := time.NewTicker(s.cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.logger.Printf("watcher loop stopping: %v", ctx.Err())
			return ctx.Err()
		case <-ticker.C:
			runCheck()
		}
	}
}

// LogCheckResult emits a concise summary log line for a completed CheckResult.
func (s *Service) LogCheckResult(res *CheckResult, elapsed time.Duration) {
	if res == nil {
		return
	}
	switch {
	case res.BaselineInitialized:
		s.logger.Printf("[check] initialized baseline master.mdb at RES-VER=%s (no notification sent, took %s)",
			res.NewResVer, elapsed)
	case res.Updated:
		s.logger.Printf("[check] updated RES-VER %s -> %s (%d table(s) changed, sent %d message(s), took %s)",
			displayVer(res.OldResVer), res.NewResVer, len(res.Diff.Tables), len(res.SentMessages), elapsed)
	default:
		s.logger.Printf("[check] RES-VER=%s unchanged (took %s)", res.NewResVer, elapsed)
	}
}

// RunOnce performs a single update check, diffing and notifying when a new RES-VER is found.
func (s *Service) RunOnce(ctx context.Context) (*CheckResult, error) {
	checkedAt := s.now().UTC()
	st, err := LoadState(s.cfg.StatePath)
	if err != nil {
		return nil, err
	}
	s.logger.Printf("[check] starting update check (last_res_ver=%s, baseline_db=%q)",
		displayVer(st.LastResVer), s.cfg.MasterDBPath)

	// Capture savedata RES-VER prior to CheckResourceVersion updating savedata.Store.
	prevSavedResVer := s.store.Data().ResVer
	seeded, err := s.ensureBaselineFromSeed()
	if err != nil {
		return nil, err
	}
	if seeded {
		if st.LastResVer == "" && st.PendingOldResVer == "" && prevSavedResVer != "" {
			st.PendingOldResVer = prevSavedResVer
			if err := SaveStateAtomic(s.cfg.StatePath, st); err != nil {
				return nil, err
			}
		}
		s.logger.Printf("[check] seeded initial baseline %q from %q (pending_old_res_ver=%s)",
			s.cfg.MasterDBPath, s.cfg.SeedMasterDBPath, displayVer(st.PendingOldResVer))
	}

	s.logger.Printf("[check] checking latest RES-VER...")
	rv, err := s.fetcher.CheckResourceVersion(ctx)
	if err != nil {
		return nil, fmt.Errorf("check resource version: %w", err)
	}
	s.logger.Printf("[check] server returned RES-VER=%s", rv.ResourceVersion)

	hasBaseline, err := fileExists(s.cfg.MasterDBPath)
	if err != nil {
		return nil, err
	}
	if hasBaseline && st.LastResVer != "" && rv.ResourceVersion == st.LastResVer {
		s.logger.Printf("[check] RES-VER=%s matches local baseline; skipping manifest and database fetches",
			rv.ResourceVersion)
		return &CheckResult{
			CheckedAt: checkedAt,
			OldResVer: st.LastResVer,
			NewResVer: rv.ResourceVersion,
			Updated:   false,
		}, nil
	}

	if !hasBaseline {
		s.logger.Printf("[check] no baseline master.mdb at %q; downloading initial baseline...", s.cfg.MasterDBPath)
		if _, err := s.resolveAndDownload(ctx, rv, s.cfg.MasterDBPath); err != nil {
			return nil, fmt.Errorf("download initial baseline master.mdb: %w", err)
		}
		if err := SaveStateAtomic(s.cfg.StatePath, State{LastResVer: rv.ResourceVersion}); err != nil {
			return nil, err
		}
		s.logger.Printf("[check] baseline saved to %q and state updated (%s = %s)",
			s.cfg.MasterDBPath, s.cfg.StatePath, rv.ResourceVersion)
		return &CheckResult{
			CheckedAt:           checkedAt,
			NewResVer:           rv.ResourceVersion,
			BaselineInitialized: true,
		}, nil
	}

	oldResVer := st.LastResVer
	if oldResVer == "" {
		oldResVer = st.PendingOldResVer
	}

	stagingPath := s.cfg.MasterDBPath + ".new"
	defer func() {
		_ = os.Remove(stagingPath)
	}()

	s.logger.Printf("[check] new RES-VER detected (%s -> %s)", displayVer(oldResVer), rv.ResourceVersion)
	info, err := s.resolveAndDownload(ctx, rv, stagingPath)
	if err != nil {
		return nil, fmt.Errorf("download new master.mdb: %w", err)
	}

	s.logger.Printf("[check] download complete; computing diff (%q vs %q)...",
		s.cfg.MasterDBPath, stagingPath)
	diff, err := mdbdiff.DiffFiles(s.cfg.MasterDBPath, stagingPath)
	if err != nil {
		return nil, fmt.Errorf("diff master.mdb: %w", err)
	}
	s.logger.Printf("[check] diff complete: %d table(s) changed, campaign_data (+%d / -%d / ~%d), single_mode_scenario (+%d / -%d / ~%d), champions_schedule (+%d / -%d / ~%d)",
		len(diff.Tables),
		len(diff.Campaign.Added), len(diff.Campaign.Deleted), len(diff.Campaign.Changed),
		len(diff.Scenario.Added), len(diff.Scenario.Deleted), len(diff.Scenario.Changed),
		len(diff.Champions.Added), len(diff.Champions.Deleted), len(diff.Champions.Changed))
	if s.cfg.Verbose {
		for _, td := range diff.Tables {
			s.logger.Printf("[check]   table %s: +%d / -%d / ~%d", td.Table, td.Added, td.Deleted, td.Changed)
		}
	}

	var sent []string
	summaryMsg := notify.FormatSummaryMessage(oldResVer, info.ResourceVersion, diff)
	s.logger.Printf("[check] sending summary notification (%d chars)...", len(summaryMsg))
	if err := s.notifier.SendMessage(ctx, summaryMsg); err != nil {
		return nil, fmt.Errorf("send summary notification: %w", err)
	}
	sent = append(sent, summaryMsg)

	sendOptional := func(label, msg string, ok bool) error {
		if !ok {
			s.logger.Printf("[check] no %s changes; skipping notification", label)
			return nil
		}
		s.logger.Printf("[check] sending %s notification (%d chars)...", label, len(msg))
		if err := s.notifier.SendMessage(ctx, msg); err != nil {
			return fmt.Errorf("send %s notification: %w", label, err)
		}
		sent = append(sent, msg)
		return nil
	}

	campaignMsg, campaignOK := notify.FormatCampaignDiffMessage(diff.Campaign, checkedAt)
	if err := sendOptional("active campaign_data", campaignMsg, campaignOK); err != nil {
		return nil, err
	}

	scheduleMsg, scheduleOK := notify.FormatScenarioAndChampionsMessage(diff.Scenario, diff.Champions, checkedAt)
	if err := sendOptional("single_mode_scenario/champions_schedule", scheduleMsg, scheduleOK); err != nil {
		return nil, err
	}

	if err := os.Rename(stagingPath, s.cfg.MasterDBPath); err != nil {
		return nil, fmt.Errorf("promote new master.mdb to %q: %w", s.cfg.MasterDBPath, err)
	}
	if err := SaveStateAtomic(s.cfg.StatePath, State{LastResVer: info.ResourceVersion}); err != nil {
		return nil, err
	}
	s.logger.Printf("[check] promoted %q -> %q and saved state (%s = %s)",
		stagingPath, s.cfg.MasterDBPath, s.cfg.StatePath, info.ResourceVersion)

	return &CheckResult{
		CheckedAt:    checkedAt,
		OldResVer:    oldResVer,
		NewResVer:    info.ResourceVersion,
		Updated:      true,
		Diff:         diff,
		SentMessages: sent,
	}, nil
}

func (s *Service) resolveAndDownload(ctx context.Context, rv *masterdb.ResourceVersionInfo, dstPath string) (*masterdb.MasterInfo, error) {
	s.logger.Printf("[check] resolving manifest chain for RES-VER=%s...", rv.ResourceVersion)
	info, err := s.fetcher.ResolveManifest(ctx, rv)
	if err != nil {
		return nil, err
	}
	s.logger.Printf("[check] manifest resolved (platform=%s, compressed_size=%d bytes); downloading to %q...",
		info.Platform, info.CompressedSize, dstPath)
	if s.cfg.Verbose && info.URL != "" {
		s.logger.Printf("[check] master.mdb.lz4 hash=%s url=%s", info.HashName, info.URL)
	}
	if err := s.fetcher.DownloadMasterInfo(ctx, info, dstPath); err != nil {
		return nil, err
	}
	return info, nil
}

func displayVer(v string) string {
	if v == "" {
		return "<none>"
	}
	return v
}

func (s *Service) ensureBaselineFromSeed() (bool, error) {
	if s.cfg.SeedMasterDBPath == "" {
		return false, nil
	}
	exists, err := fileExists(s.cfg.MasterDBPath)
	if err != nil || exists {
		return false, err
	}
	if err := copyFileAtomic(s.cfg.SeedMasterDBPath, s.cfg.MasterDBPath); err != nil {
		return false, fmt.Errorf("seed baseline master.mdb from %q: %w", s.cfg.SeedMasterDBPath, err)
	}
	return true, nil
}

func fileExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func copyFileAtomic(srcPath, dstPath string) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()

	dir := filepath.Dir(dstPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, ".seed-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := io.Copy(tmp, src); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, dstPath); err != nil {
		return err
	}
	cleanup = false
	return nil
}
