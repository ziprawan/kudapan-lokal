package discordbot

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"kudapan_lokal/internal/mdbdiff"
	"kudapan_lokal/internal/watcher"
)

// DefaultInterval is the default schedule check interval (once per hour).
const DefaultInterval = time.Hour

// MessageSender sends a notification message to a Discord channel.
type MessageSender interface {
	SendChannelMessage(ctx context.Context, channelID, content, roleID string) error
}

// SchedulerConfig configures the Champions Meeting Discord notification scheduler.
type SchedulerConfig struct {
	MasterDBPath string
	Interval     time.Duration
	CheckOnStart bool
}

// SchedulerOption customizes a Scheduler instance.
type SchedulerOption func(*Scheduler)

// WithLogger sets the logger used by Scheduler.
func WithLogger(l *log.Logger) SchedulerOption {
	return func(s *Scheduler) {
		if l != nil {
			s.logger = l
		}
	}
}

// WithClock overrides the clock used by Scheduler.
func WithClock(now func() time.Time) SchedulerOption {
	return func(s *Scheduler) {
		if now != nil {
			s.now = now
		}
	}
}

// Scheduler periodically checks master.mdb for Champions Meeting round starts
// and broadcasts notifications to configured Discord guild channels.
type Scheduler struct {
	cfg    SchedulerConfig
	store  *Store
	sender MessageSender
	logger *log.Logger
	now    func() time.Time
}

// NewScheduler constructs a Champions Meeting Discord notification Scheduler.
func NewScheduler(cfg SchedulerConfig, store *Store, sender MessageSender, opts ...SchedulerOption) (*Scheduler, error) {
	if store == nil {
		return nil, errors.New("discord store cannot be nil")
	}
	if sender == nil {
		return nil, errors.New("discord message sender cannot be nil")
	}
	if strings.TrimSpace(cfg.MasterDBPath) == "" {
		cfg.MasterDBPath = watcher.DefaultMasterDBPath
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}

	s := &Scheduler{
		cfg:    cfg,
		store:  store,
		sender: sender,
		logger: log.Default(),
		now:    time.Now,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	return s, nil
}

// Run runs the hourly Champions Meeting check loop aligned via watcher.NextAlignedTime
// until ctx is canceled.
func (s *Scheduler) Run(ctx context.Context) error {
	s.logger.Printf("discord scheduler started (interval=%s, check_on_start=%v, db=%q)",
		s.cfg.Interval, s.cfg.CheckOnStart, s.cfg.MasterDBPath)

	runOnce := func() {
		sent, err := s.RunOnce(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.logger.Printf("[discord-schedule] check failed: %v", err)
			return
		}
		if sent > 0 {
			s.logger.Printf("[discord-schedule] sent %d Champions Meeting notification(s)", sent)
		}
	}

	if s.cfg.CheckOnStart {
		runOnce()
		if ctx.Err() != nil {
			s.logger.Printf("discord scheduler stopping: %v", ctx.Err())
			return ctx.Err()
		}
	}

	var lastTarget time.Time
	for {
		now := s.now()
		nextAt := watcher.NextAlignedTime(now, s.cfg.Interval)
		if !lastTarget.IsZero() && !nextAt.After(lastTarget) {
			nextAt = lastTarget.Add(s.cfg.Interval)
		}
		lastTarget = nextAt

		wait := nextAt.Sub(now)
		if wait < 0 {
			wait = 0
		}
		s.logger.Printf("[discord-schedule] next check scheduled at %s (in %s)",
			nextAt.Format("2006-01-02 15:04:05 MST"), wait.Round(time.Second))

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			s.logger.Printf("discord scheduler stopping: %v", ctx.Err())
			return ctx.Err()
		case <-timer.C:
			runOnce()
		}
	}
}

// RunOnce checks master.mdb for unnotified Champions Meeting round starts and
// broadcasts any active milestone notifications to all configured Discord guilds.
func (s *Scheduler) RunOnce(ctx context.Context) (int, error) {
	if _, err := os.Stat(s.cfg.MasterDBPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}

	now := s.now().UTC()
	lastID, ok, err := s.store.LastNotifiedID(ctx, EventTypeChampionsMeeting)
	if err != nil {
		return 0, err
	}
	if !ok {
		seedID, err := mdbdiff.MaxPastChampionsRoundMilestoneID(s.cfg.MasterDBPath, now)
		if err != nil {
			return 0, err
		}
		if _, err := s.store.EnsureLastNotifiedID(ctx, EventTypeChampionsMeeting, seedID); err != nil {
			return 0, err
		}
		s.logger.Printf("[discord-schedule] initialized %s cursor at id=%d", EventTypeChampionsMeeting, seedID)
		return 0, nil
	}

	milestones, err := mdbdiff.LoadChampionsRoundMilestonesAfter(s.cfg.MasterDBPath, lastID)
	if err != nil {
		return 0, fmt.Errorf("load champions round milestones: %w", err)
	}
	if len(milestones) == 0 {
		return 0, nil
	}

	guilds, err := s.store.ListGuilds(ctx)
	if err != nil {
		return 0, err
	}

	sentCount := 0
	for _, m := range milestones {
		if now.Before(m.StartTime) {
			break
		}
		if !now.Before(m.EndTime) {
			if err := s.store.SetLastNotifiedID(ctx, EventTypeChampionsMeeting, m.ID); err != nil {
				return sentCount, err
			}
			continue
		}

		for _, g := range guilds {
			msg := FormatChampionsRoundMessage(m, g.RoleID)
			if err := s.sender.SendChannelMessage(ctx, g.ChannelID, msg, g.RoleID); err != nil {
				return sentCount, fmt.Errorf("notify guild %q channel %q: %w", g.GuildID, g.ChannelID, err)
			}
			sentCount++
		}

		if err := s.store.SetLastNotifiedID(ctx, EventTypeChampionsMeeting, m.ID); err != nil {
			return sentCount, err
		}
	}
	return sentCount, nil
}

// FormatChampionsRoundMessage formats a Discord notification for a Champions
// Meeting round start milestone, prepending a role mention when roleID is set.
func FormatChampionsRoundMessage(m mdbdiff.ChampionsRoundMilestone, roleID string) string {
	var sb strings.Builder
	if roleID := strings.TrimSpace(roleID); roleID != "" {
		fmt.Fprintf(&sb, "<@&%s>\n", roleID)
	}

	title := strings.TrimSpace(m.Title)
	if title == "" {
		title = fmt.Sprintf("Champions Meeting #%d", m.ChampionsID)
	}
	fmt.Fprintf(&sb, "**[Champions Meeting] #%d %s — %s**", m.ChampionsID, title, m.RoundLabel())
	if course := strings.TrimSpace(m.Course); course != "" {
		fmt.Fprintf(&sb, "\nCourse: %s", course)
	}
	startUnix := m.StartTime.UTC().Unix()
	endUnix := m.EndTime.UTC().Unix()
	fmt.Fprintf(&sb, "\nStart: <t:%d:f> (<t:%d:R>)", startUnix, startUnix)
	fmt.Fprintf(&sb, "\nEnd: <t:%d:f> (<t:%d:R>)", endUnix, endUnix)
	return sb.String()
}
