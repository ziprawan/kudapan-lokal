// Package discordbot implements Discord Champions Meeting round schedule
// notifications, guild configuration persistence in SQLite, slash command
// handlers, and Discord REST API integration.
package discordbot

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	// DefaultDBPath is the default path for the dedicated Discord bot SQLite database.
	DefaultDBPath = "data/discord.db"
	// EventTypeChampionsMeeting identifies the Champions Meeting tracker entry.
	EventTypeChampionsMeeting = "champions_meeting"
)

// GuildConfig represents a Discord server's configured notification destination.
type GuildConfig struct {
	GuildID   string
	ChannelID string
	RoleID    string
	UpdatedAt time.Time
}

// Store persists Discord guild notification targets and global event cursors in SQLite.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// StoreOption configures a Store instance.
type StoreOption func(*Store)

// WithStoreClock overrides the clock used for updated_at timestamps.
func WithStoreClock(now func() time.Time) StoreOption {
	return func(s *Store) {
		if now != nil {
			s.now = now
		}
	}
}

// OpenStore opens (or initializes) the Discord SQLite database at dbPath and
// ensures the discord_guilds and event_tracker tables exist.
func OpenStore(dbPath string, opts ...StoreOption) (*Store, error) {
	dbPath = strings.TrimSpace(dbPath)
	if dbPath == "" {
		dbPath = DefaultDBPath
	}

	dir := filepath.Dir(dbPath)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create discord db directory %q: %w", dir, err)
		}
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open discord db %q: %w", dbPath, err)
	}
	db.SetMaxOpenConns(1)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping discord db %q: %w", dbPath, err)
	}

	stmts := []string{
		`PRAGMA busy_timeout = 5000`,
		`CREATE TABLE IF NOT EXISTS discord_guilds (
			guild_id   TEXT PRIMARY KEY,
			channel_id TEXT NOT NULL,
			role_id    TEXT NOT NULL DEFAULT '',
			updated_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS event_tracker (
			event_type       TEXT PRIMARY KEY,
			last_notified_id INTEGER NOT NULL,
			updated_at       INTEGER NOT NULL
		)`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("init discord db schema: %w", err)
		}
	}

	s := &Store{
		db:  db,
		now: time.Now,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	return s, nil
}

// Close closes the underlying SQLite connection.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// UpsertGuild saves or replaces the notification channel and optional role for guildID.
func (s *Store) UpsertGuild(ctx context.Context, guildID, channelID, roleID string) (GuildConfig, error) {
	guildID = strings.TrimSpace(guildID)
	channelID = strings.TrimSpace(channelID)
	roleID = strings.TrimSpace(roleID)
	if guildID == "" {
		return GuildConfig{}, errors.New("guild_id cannot be empty")
	}
	if channelID == "" {
		return GuildConfig{}, errors.New("channel_id cannot be empty")
	}

	nowUTC := s.now().UTC()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO discord_guilds (guild_id, channel_id, role_id, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(guild_id) DO UPDATE SET
			channel_id = excluded.channel_id,
			role_id    = excluded.role_id,
			updated_at = excluded.updated_at`,
		guildID, channelID, roleID, nowUTC.Unix(),
	)
	if err != nil {
		return GuildConfig{}, fmt.Errorf("upsert discord guild %q: %w", guildID, err)
	}

	return GuildConfig{
		GuildID:   guildID,
		ChannelID: channelID,
		RoleID:    roleID,
		UpdatedAt: nowUTC,
	}, nil
}

// ListGuilds returns all configured Discord guilds ordered by guild_id.
func (s *Store) ListGuilds(ctx context.Context) ([]GuildConfig, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT guild_id, channel_id, role_id, updated_at
		FROM discord_guilds
		ORDER BY guild_id ASC`)
	if err != nil {
		return nil, fmt.Errorf("list discord guilds: %w", err)
	}
	defer rows.Close()

	var out []GuildConfig
	for rows.Next() {
		var g GuildConfig
		var updatedUnix int64
		if err := rows.Scan(&g.GuildID, &g.ChannelID, &g.RoleID, &updatedUnix); err != nil {
			return nil, err
		}
		g.UpdatedAt = time.Unix(updatedUnix, 0).UTC()
		out = append(out, g)
	}
	return out, rows.Err()
}

// LastNotifiedID returns the stored cursor for eventType, or (0, false, nil) if unset.
func (s *Store) LastNotifiedID(ctx context.Context, eventType string) (int64, bool, error) {
	eventType = strings.TrimSpace(eventType)
	if eventType == "" {
		return 0, false, errors.New("event_type cannot be empty")
	}

	var id int64
	err := s.db.QueryRowContext(ctx, `
		SELECT last_notified_id
		FROM event_tracker
		WHERE event_type = ?`, eventType).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("query event_tracker %q: %w", eventType, err)
	}
	return id, true, nil
}

// EnsureLastNotifiedID initializes eventType with seedID if no cursor exists yet,
// returning the resulting stored cursor value.
func (s *Store) EnsureLastNotifiedID(ctx context.Context, eventType string, seedID int64) (int64, error) {
	eventType = strings.TrimSpace(eventType)
	if eventType == "" {
		return 0, errors.New("event_type cannot be empty")
	}

	nowUnix := s.now().UTC().Unix()
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO event_tracker (event_type, last_notified_id, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(event_type) DO NOTHING`,
		eventType, seedID, nowUnix,
	); err != nil {
		return 0, fmt.Errorf("seed event_tracker %q: %w", eventType, err)
	}

	id, _, err := s.LastNotifiedID(ctx, eventType)
	return id, err
}

// SetLastNotifiedID updates or inserts the global cursor for eventType.
func (s *Store) SetLastNotifiedID(ctx context.Context, eventType string, id int64) error {
	eventType = strings.TrimSpace(eventType)
	if eventType == "" {
		return errors.New("event_type cannot be empty")
	}

	nowUnix := s.now().UTC().Unix()
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO event_tracker (event_type, last_notified_id, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(event_type) DO UPDATE SET
			last_notified_id = excluded.last_notified_id,
			updated_at       = excluded.updated_at`,
		eventType, id, nowUnix,
	); err != nil {
		return fmt.Errorf("update event_tracker %q: %w", eventType, err)
	}
	return nil
}
