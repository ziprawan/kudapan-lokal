package discordbot

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"kudapan_lokal/internal/mdbdiff"
	"kudapan_lokal/internal/webhook"
)

type fakeVerifier struct {
	err       error
	calls     int
	lastGuild string
	lastChan  string
	lastRole  string
}

func (f *fakeVerifier) VerifyNotifyPermissions(_ context.Context, guildID, channelID, roleID string, _ webhook.DiscordInteraction) error {
	f.calls++
	f.lastGuild = guildID
	f.lastChan = channelID
	f.lastRole = roleID
	return f.err
}

type sentMessage struct {
	channelID string
	content   string
	roleID    string
}

type fakeSender struct {
	messages []sentMessage
	failOn   int
	attempts int
}

func (f *fakeSender) SendChannelMessage(_ context.Context, channelID, content, roleID string) error {
	f.attempts++
	if f.failOn > 0 && f.attempts == f.failOn {
		return errors.New("simulated discord send error")
	}
	f.messages = append(f.messages, sentMessage{
		channelID: channelID,
		content:   content,
		roleID:    roleID,
	})
	return nil
}

func TestStoreGuildsAndEventTracker(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sub", "discord.db")
	now := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)

	store, err := OpenStore(dbPath, WithStoreClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("OpenStore() error = %v", err)
	}
	defer store.Close()

	if _, ok, err := store.LastNotifiedID(context.Background(), EventTypeChampionsMeeting); err != nil || ok {
		t.Fatalf("expected unset event_tracker, got ok=%v err=%v", ok, err)
	}

	seeded, err := store.EnsureLastNotifiedID(context.Background(), EventTypeChampionsMeeting, 42)
	if err != nil || seeded != 42 {
		t.Fatalf("EnsureLastNotifiedID(42) = (%d, %v), want (42, nil)", seeded, err)
	}
	// Second EnsureLastNotifiedID does not overwrite existing row.
	seeded2, err := store.EnsureLastNotifiedID(context.Background(), EventTypeChampionsMeeting, 99)
	if err != nil || seeded2 != 42 {
		t.Fatalf("EnsureLastNotifiedID(99) = (%d, %v), want (42, nil)", seeded2, err)
	}

	if err := store.SetLastNotifiedID(context.Background(), EventTypeChampionsMeeting, 55); err != nil {
		t.Fatalf("SetLastNotifiedID(55) error = %v", err)
	}
	gotID, ok, err := store.LastNotifiedID(context.Background(), EventTypeChampionsMeeting)
	if err != nil || !ok || gotID != 55 {
		t.Fatalf("LastNotifiedID() = (%d, %v, %v), want (55, true, nil)", gotID, ok, err)
	}

	if _, err := store.UpsertGuild(context.Background(), "g2", "c2", ""); err != nil {
		t.Fatalf("UpsertGuild(g2) error = %v", err)
	}
	if _, err := store.UpsertGuild(context.Background(), "g1", "c1", "r1"); err != nil {
		t.Fatalf("UpsertGuild(g1) error = %v", err)
	}
	if _, err := store.UpsertGuild(context.Background(), "g1", "c1-new", "r1-new"); err != nil {
		t.Fatalf("UpsertGuild(g1 update) error = %v", err)
	}

	guilds, err := store.ListGuilds(context.Background())
	if err != nil {
		t.Fatalf("ListGuilds() error = %v", err)
	}
	if len(guilds) != 2 || guilds[0].GuildID != "g1" || guilds[0].ChannelID != "c1-new" || guilds[0].RoleID != "r1-new" || guilds[1].GuildID != "g2" {
		t.Fatalf("unexpected ListGuilds(): %+v", guilds)
	}

	if _, err := store.UpsertResourceGuild(context.Background(), "g1", "rc1", "rr1"); err != nil {
		t.Fatalf("UpsertResourceGuild(g1) error = %v", err)
	}
	resGuilds, err := store.ListResourceGuilds(context.Background())
	if err != nil || len(resGuilds) != 1 || resGuilds[0].ChannelID != "rc1" || resGuilds[0].RoleID != "rr1" {
		t.Fatalf("unexpected ListResourceGuilds(): (%+v, %v)", resGuilds, err)
	}
	// Ensure resource guilds did not alter event guilds.
	if guildsAfter, _ := store.ListGuilds(context.Background()); len(guildsAfter) != 2 || guildsAfter[0].ChannelID != "c1-new" {
		t.Fatalf("expected discord_guilds to remain independent, got %+v", guildsAfter)
	}
}

func TestNotifyChannelCommandAdminAndPermissions(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "discord.db"))
	if err != nil {
		t.Fatalf("OpenStore() error = %v", err)
	}
	defer store.Close()

	masterPath := filepath.Join(dir, "master.mdb")
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	createChampionsScheduleTestDB(t, masterPath, now)

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey() error = %v", err)
	}

	verifier := &fakeVerifier{}
	dcHandler, err := webhook.NewDiscordHandler(webhook.DiscordConfig{
		PublicKey: pub,
		Logger:    log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatalf("NewDiscordHandler() error = %v", err)
	}
	RegisterCommands(dcHandler, store, verifier, masterPath, func() time.Time { return now })

	sendInteraction := func(ix webhook.DiscordInteraction) webhook.DiscordInteractionResponse {
		body, err := json.Marshal(ix)
		if err != nil {
			t.Fatalf("marshal interaction: %v", err)
		}
		ts := "1700000000"
		sig := hex.EncodeToString(ed25519.Sign(priv, append([]byte(ts), body...)))
		req := httptest.NewRequest(http.MethodPost, "/api/webhook/discord", bytes.NewReader(body))
		req.Header.Set("X-Signature-Ed25519", sig)
		req.Header.Set("X-Signature-Timestamp", ts)
		rec := httptest.NewRecorder()
		dcHandler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		var resp webhook.DiscordInteractionResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal response: %v", err)
		}
		return resp
	}

	// Non-admin user is rejected before permission check or DB write.
	nonAdminResp := sendInteraction(webhook.DiscordInteraction{
		ID:      "ix1",
		Type:    webhook.DiscordInteractionApplicationCommand,
		GuildID: "guild-1",
		Member:  &webhook.DiscordInteractionMember{Permissions: strconv.FormatUint(PermViewChannel|PermSendMessages, 10)},
		Data: &webhook.DiscordInteractionData{
			Name:    "notify-event",
			Options: []webhook.DiscordInteractionOption{{Name: "channel", Type: 7, Value: "chan-1"}},
		},
	})
	if nonAdminResp.Data == nil || !strings.Contains(nonAdminResp.Data.Content, "Only server administrators") {
		t.Fatalf("expected admin rejection, got: %+v", nonAdminResp)
	}
	if verifier.calls != 0 {
		t.Errorf("expected verifier not to run for non-admin, got %d calls", verifier.calls)
	}

	// Bot permission failure rejects cleanly without writing guild to SQLite.
	verifier.err = errors.New("bot lacks Send Messages permission in <#chan-1>")
	permFailResp := sendInteraction(webhook.DiscordInteraction{
		ID:      "ix2",
		Type:    webhook.DiscordInteractionApplicationCommand,
		GuildID: "guild-1",
		Member:  &webhook.DiscordInteractionMember{Permissions: strconv.FormatUint(PermAdministrator, 10)},
		Data: &webhook.DiscordInteractionData{
			Name:    "notify-event",
			Options: []webhook.DiscordInteractionOption{{Name: "channel", Type: 7, Value: "chan-1"}},
		},
	})
	if permFailResp.Data == nil || !strings.Contains(permFailResp.Data.Content, "Permission check failed") {
		t.Fatalf("expected permission failure reply, got: %+v", permFailResp)
	}
	if guilds, _ := store.ListGuilds(context.Background()); len(guilds) != 0 {
		t.Fatalf("expected no guilds saved on permission failure, got %+v", guilds)
	}

	// Admin + passing permission check saves guild and seeds event_tracker.
	verifier.err = nil
	okResp := sendInteraction(webhook.DiscordInteraction{
		ID:      "ix3",
		Type:    webhook.DiscordInteractionApplicationCommand,
		GuildID: "guild-1",
		Member:  &webhook.DiscordInteractionMember{Permissions: strconv.FormatUint(PermAdministrator, 10)},
		Data: &webhook.DiscordInteractionData{
			Name: "notify-event",
			Options: []webhook.DiscordInteractionOption{
				{Name: "channel", Type: 7, Value: "chan-1"},
				{Name: "role", Type: 8, Value: "role-1"},
			},
		},
	})
	if okResp.Data == nil || !strings.Contains(okResp.Data.Content, "<#chan-1>") || !strings.Contains(okResp.Data.Content, "<@&role-1>") {
		t.Fatalf("unexpected success reply: %+v", okResp)
	}
	guilds, err := store.ListGuilds(context.Background())
	if err != nil || len(guilds) != 1 || guilds[0].ChannelID != "chan-1" || guilds[0].RoleID != "role-1" {
		t.Fatalf("saved guilds = (%+v, %v)", guilds, err)
	}
	// Seeded to ID 2 (Round 1 already started at now-1h).
	lastID, ok, err := store.LastNotifiedID(context.Background(), EventTypeChampionsMeeting)
	if err != nil || !ok || lastID != 2 {
		t.Fatalf("event_tracker = (%d, %v, %v), want (2, true, nil)", lastID, ok, err)
	}
}

func TestClientSyncVerifyPermissionsAndSendMessage(t *testing.T) {
	var syncedBody []applicationCommand
	var sentBody createMessageRequest

	targetChannelOverwrites := []DiscordPermissionOverwrite{}
	targetRoleMentionable := true
	botGuildRoles := []string{"role-bot"}
	botRolePerms := strconv.FormatUint(PermViewChannel|PermSendMessages, 10)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bot bot-secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/applications/app-1/commands":
			raw, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(raw, &syncedBody); err != nil {
				t.Fatalf("unmarshal commands: %v", err)
			}
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodGet && r.URL.Path == "/channels/chan-target":
			_ = json.NewEncoder(w).Encode(discordChannel{
				ID:                   "chan-target",
				GuildID:              "guild-1",
				PermissionOverwrites: targetChannelOverwrites,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/guilds/guild-1/roles":
			_ = json.NewEncoder(w).Encode([]DiscordRole{
				{ID: "guild-1", Name: "@everyone", Permissions: "0"},
				{ID: "role-bot", Name: "Bot", Permissions: botRolePerms},
				{ID: "role-ping", Name: "CM Ping", Permissions: "0", Mentionable: targetRoleMentionable},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/guilds/guild-1/members/app-1":
			_ = json.NewEncoder(w).Encode(discordMember{Roles: botGuildRoles})
		case r.Method == http.MethodPost && r.URL.Path == "/channels/chan-target/messages":
			raw, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(raw, &sentBody); err != nil {
				t.Fatalf("unmarshal create message: %v", err)
			}
			_, _ = w.Write([]byte(`{"id":"msg-1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client, err := NewClient("app-1", "bot-secret", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	if err := client.SyncCommands(context.Background()); err != nil {
		t.Fatalf("SyncCommands() error = %v", err)
	}
	if len(syncedBody) != 3 || syncedBody[1].Name != "notify-event" || syncedBody[2].Name != "notify-resource" ||
		syncedBody[1].DefaultMemberPermissions == nil || *syncedBody[1].DefaultMemberPermissions != "8" ||
		syncedBody[2].DefaultMemberPermissions == nil || *syncedBody[2].DefaultMemberPermissions != "8" {
		t.Fatalf("unexpected synced commands: %+v", syncedBody)
	}

	ix := webhook.DiscordInteraction{GuildID: "guild-1", ChannelID: "chan-other"}

	// Case 1: bot has VIEW_CHANNEL | SEND_MESSAGES and role is mentionable -> passes.
	if err := client.VerifyNotifyPermissions(context.Background(), "guild-1", "chan-target", "role-ping", ix); err != nil {
		t.Fatalf("expected mentionable role + send perms to pass, got: %v", err)
	}

	// Case 2: role is NOT mentionable and bot lacks MENTION_EVERYONE -> rejected.
	targetRoleMentionable = false
	if err := client.VerifyNotifyPermissions(context.Background(), "guild-1", "chan-target", "role-ping", ix); err == nil || !strings.Contains(err.Error(), "not mentionable") {
		t.Fatalf("expected unmentionable role rejection, got: %v", err)
	}

	// Case 3: role is NOT mentionable, but bot has MENTION_EVERYONE overwrite -> passes.
	targetChannelOverwrites = []DiscordPermissionOverwrite{
		{ID: "role-bot", Type: 0, Allow: strconv.FormatUint(PermMentionEveryone, 10), Deny: "0"},
	}
	if err := client.VerifyNotifyPermissions(context.Background(), "guild-1", "chan-target", "role-ping", ix); err != nil {
		t.Fatalf("expected MENTION_EVERYONE overwrite to allow role tag, got: %v", err)
	}

	// Case 4: channel member overwrite denies SEND_MESSAGES -> rejected.
	targetChannelOverwrites = []DiscordPermissionOverwrite{
		{ID: "app-1", Type: 1, Allow: "0", Deny: strconv.FormatUint(PermSendMessages, 10)},
	}
	if err := client.VerifyNotifyPermissions(context.Background(), "guild-1", "chan-target", "", ix); err == nil || !strings.Contains(err.Error(), "Send Messages") {
		t.Fatalf("expected denied SEND_MESSAGES rejection, got: %v", err)
	}

	// Verify SendChannelMessage payload and allowed_mentions.
	if err := client.SendChannelMessage(context.Background(), "chan-target", "<@&role-ping>\nHello!", "role-ping"); err != nil {
		t.Fatalf("SendChannelMessage() error = %v", err)
	}
	if sentBody.Content != "<@&role-ping>\nHello!" || sentBody.AllowedMentions == nil ||
		len(sentBody.AllowedMentions.Roles) != 1 || sentBody.AllowedMentions.Roles[0] != "role-ping" ||
		len(sentBody.AllowedMentions.Parse) != 0 {
		t.Fatalf("unexpected createMessage payload: %+v", sentBody)
	}
}

func TestSchedulerRoundsAndGlobalCursorProgression(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "discord.db"))
	if err != nil {
		t.Fatalf("OpenStore() error = %v", err)
	}
	defer store.Close()

	masterPath := filepath.Join(dir, "master.mdb")
	base := time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)
	// Round 1 starts at base+1h, Round 2 at base+25h, Round 3.1 at base+49h, Round 3.2 at base+60h (skipped), Round 3.3 at base+73h.
	createChampionsScheduleTestDB(t, masterPath, base.Add(2*time.Hour))

	if _, err := store.UpsertGuild(context.Background(), "g1", "chan-1", "role-1"); err != nil {
		t.Fatalf("UpsertGuild(g1) error = %v", err)
	}
	if _, err := store.UpsertGuild(context.Background(), "g2", "chan-2", ""); err != nil {
		t.Fatalf("UpsertGuild(g2) error = %v", err)
	}

	curTime := base
	sender := &fakeSender{}
	sched, err := NewScheduler(SchedulerConfig{
		MasterDBPath: masterPath,
		Interval:     time.Hour,
	}, store, sender, WithClock(func() time.Time { return curTime }), WithLogger(log.New(io.Discard, "", 0)))
	if err != nil {
		t.Fatalf("NewScheduler() error = %v", err)
	}

	// First RunOnce initializes cursor at 0 (no rounds started at base) and sends 0 messages.
	sent, err := sched.RunOnce(context.Background())
	if err != nil || sent != 0 {
		t.Fatalf("initial RunOnce() = (%d, %v), want (0, nil)", sent, err)
	}
	if id, ok, _ := store.LastNotifiedID(context.Background(), EventTypeChampionsMeeting); !ok || id != 0 {
		t.Fatalf("initial cursor = (%d, %v), want (0, true)", id, ok)
	}

	// At base+1h: Round 1 starts -> sends to both guilds and advances cursor to ID 2.
	curTime = base.Add(1 * time.Hour)
	sent, err = sched.RunOnce(context.Background())
	if err != nil || sent != 2 {
		t.Fatalf("Round 1 RunOnce() = (%d, %v), want (2, nil)", sent, err)
	}
	wantStart := "Start: <t:" + itoa(base.Add(1*time.Hour).Unix()) + ":f> (<t:" + itoa(base.Add(1*time.Hour).Unix()) + ":R>)"
	wantEnd := "End: <t:" + itoa(base.Add(25*time.Hour).Unix()) + ":f> (<t:" + itoa(base.Add(25*time.Hour).Unix()) + ":R>)"
	if !strings.Contains(sender.messages[0].content, "Round 1") ||
		!strings.HasPrefix(sender.messages[0].content, "<@&role-1>") ||
		!strings.Contains(sender.messages[0].content, wantStart) ||
		!strings.Contains(sender.messages[0].content, wantEnd) {
		t.Errorf("unexpected guild 1 Round 1 message:\n%s", sender.messages[0].content)
	}
	if strings.Contains(sender.messages[1].content, "<@&") {
		t.Errorf("guild 2 without role should not include role mention:\n%s", sender.messages[1].content)
	}
	if id, _, _ := store.LastNotifiedID(context.Background(), EventTypeChampionsMeeting); id != 2 {
		t.Errorf("cursor after Round 1 = %d, want 2", id)
	}

	// Re-running at base+2h does not duplicate Round 1.
	curTime = base.Add(2 * time.Hour)
	sender.messages = nil
	if sent, err := sched.RunOnce(context.Background()); err != nil || sent != 0 {
		t.Fatalf("duplicate check = (%d, %v), want (0, nil)", sent, err)
	}

	// At base+25h: Round 2 starts -> sends to both guilds, cursor = 3.
	curTime = base.Add(25 * time.Hour)
	sent, err = sched.RunOnce(context.Background())
	if err != nil || sent != 2 || !strings.Contains(sender.messages[0].content, "Round 2") {
		t.Fatalf("Round 2 RunOnce() = (%d, %v), msgs=%+v", sent, err, sender.messages)
	}

	// At base+49h: Round 3.1 Registration starts -> sends, cursor = 4.
	curTime = base.Add(49 * time.Hour)
	sender.messages = nil
	sent, err = sched.RunOnce(context.Background())
	if err != nil || sent != 2 || !strings.Contains(sender.messages[0].content, "Round 3 (Registration)") {
		t.Fatalf("Round 3.1 RunOnce() = (%d, %v), msgs=%+v", sent, err, sender.messages)
	}

	// At base+60h: Round 3.2 Matching starts -> skipped completely (0 messages sent).
	curTime = base.Add(60 * time.Hour)
	sender.messages = nil
	if sent, err := sched.RunOnce(context.Background()); err != nil || sent != 0 {
		t.Fatalf("Round 3.2 RunOnce() = (%d, %v), want (0, nil)", sent, err)
	}

	// At base+73h: Round 3.3 Finals starts -> sends, cursor = 6.
	curTime = base.Add(73 * time.Hour)
	sender.messages = nil
	sent, err = sched.RunOnce(context.Background())
	if err != nil || sent != 2 || !strings.Contains(sender.messages[0].content, "Round 3 (Finals)") {
		t.Fatalf("Round 3.3 RunOnce() = (%d, %v), msgs=%+v", sent, err, sender.messages)
	}
	if id, _, _ := store.LastNotifiedID(context.Background(), EventTypeChampionsMeeting); id != 6 {
		t.Errorf("cursor after Round 3.3 = %d, want 6", id)
	}
}

func TestNotifyResourceCommandAdminAndPermissions(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "discord.db"))
	if err != nil {
		t.Fatalf("OpenStore() error = %v", err)
	}
	defer store.Close()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey() error = %v", err)
	}
	verifier := &fakeVerifier{}
	dcHandler, err := webhook.NewDiscordHandler(webhook.DiscordConfig{
		PublicKey: pub,
		Logger:    log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatalf("NewDiscordHandler() error = %v", err)
	}
	RegisterCommands(dcHandler, store, verifier, "", time.Now)

	send := func(ix webhook.DiscordInteraction) webhook.DiscordInteractionResponse {
		body, _ := json.Marshal(ix)
		ts := "1700000000"
		sig := hex.EncodeToString(ed25519.Sign(priv, append([]byte(ts), body...)))
		req := httptest.NewRequest(http.MethodPost, "/api/webhook/discord", bytes.NewReader(body))
		req.Header.Set("X-Signature-Ed25519", sig)
		req.Header.Set("X-Signature-Timestamp", ts)
		rec := httptest.NewRecorder()
		dcHandler.ServeHTTP(rec, req)
		var resp webhook.DiscordInteractionResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal response: %v", err)
		}
		return resp
	}

	nonAdmin := send(webhook.DiscordInteraction{
		ID:      "r1",
		Type:    webhook.DiscordInteractionApplicationCommand,
		GuildID: "guild-1",
		Member:  &webhook.DiscordInteractionMember{Permissions: strconv.FormatUint(PermSendMessages, 10)},
		Data: &webhook.DiscordInteractionData{
			Name:    "notify-resource",
			Options: []webhook.DiscordInteractionOption{{Name: "channel", Type: 7, Value: "chan-res"}},
		},
	})
	if nonAdmin.Data == nil || !strings.Contains(nonAdmin.Data.Content, "Only server administrators") {
		t.Fatalf("expected non-admin rejection, got %+v", nonAdmin)
	}

	verifier.err = errors.New("bot lacks View Channel")
	permFail := send(webhook.DiscordInteraction{
		ID:      "r2",
		Type:    webhook.DiscordInteractionApplicationCommand,
		GuildID: "guild-1",
		Member:  &webhook.DiscordInteractionMember{Permissions: strconv.FormatUint(PermAdministrator, 10)},
		Data: &webhook.DiscordInteractionData{
			Name:    "notify-resource",
			Options: []webhook.DiscordInteractionOption{{Name: "channel", Type: 7, Value: "chan-res"}},
		},
	})
	if permFail.Data == nil || !strings.Contains(permFail.Data.Content, "Permission check failed") {
		t.Fatalf("expected permission failure, got %+v", permFail)
	}
	if got, _ := store.ListResourceGuilds(context.Background()); len(got) != 0 {
		t.Fatalf("expected empty resource guilds on permission failure, got %+v", got)
	}

	verifier.err = nil
	okResp := send(webhook.DiscordInteraction{
		ID:      "r3",
		Type:    webhook.DiscordInteractionApplicationCommand,
		GuildID: "guild-1",
		Member:  &webhook.DiscordInteractionMember{Permissions: strconv.FormatUint(PermAdministrator, 10)},
		Data: &webhook.DiscordInteractionData{
			Name: "notify-resource",
			Options: []webhook.DiscordInteractionOption{
				{Name: "channel", Type: 7, Value: "chan-res"},
				{Name: "role", Type: 8, Value: "role-res"},
			},
		},
	})
	if okResp.Data == nil || !strings.Contains(okResp.Data.Content, "<#chan-res>") || !strings.Contains(okResp.Data.Content, "<@&role-res>") {
		t.Fatalf("unexpected success reply: %+v", okResp)
	}
	got, err := store.ListResourceGuilds(context.Background())
	if err != nil || len(got) != 1 || got[0].ChannelID != "chan-res" || got[0].RoleID != "role-res" {
		t.Fatalf("unexpected ListResourceGuilds(): (%+v, %v)", got, err)
	}
	if eventGuilds, _ := store.ListGuilds(context.Background()); len(eventGuilds) != 0 {
		t.Fatalf("expected /notify-event guilds untouched, got %+v", eventGuilds)
	}
}

func TestFormatResourceUpdateMessageAndStartTimeOrder(t *testing.T) {
	if msg, ok := FormatResourceUpdateMessage(nil, ""); ok || msg != "" {
		t.Fatalf("expected nil diff -> empty, got (%q, %v)", msg, ok)
	}

	// Deleted-only diff should produce no notification.
	deletedOnly := &mdbdiff.DiffResult{
		Scenario: mdbdiff.ScenarioDiff{
			Deleted: []mdbdiff.ScenarioEvent{{ID: 1, Title: "Old"}},
		},
		Campaign: mdbdiff.CampaignDiff{
			Deleted: []mdbdiff.CampaignEvent{{CampaignID: 2, Description: "Expired"}},
		},
	}
	if msg, ok := FormatResourceUpdateMessage(deletedOnly, "123"); ok || msg != "" {
		t.Fatalf("expected delete-only diff -> empty, got (%q, %v)", msg, ok)
	}

	t0 := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	diff := &mdbdiff.DiffResult{
		Scenario: mdbdiff.ScenarioDiff{
			Added: []mdbdiff.ScenarioEvent{
				{ID: 10, Title: "Later\nScenario", StartTime: t0.Add(48 * time.Hour), EndTime: t0.Add(100 * time.Hour)},
				{ID: 20, Title: "Earlier\nScenario", StartTime: t0.Add(12 * time.Hour), EndTime: t0.Add(100 * time.Hour)},
			},
		},
		Champions: mdbdiff.ChampionsDiff{
			Added: []mdbdiff.ChampionsScheduleEvent{
				{ID: 22, Title: "Aquarius Cup", Course: "Tokyo Dirt 1600m Left (Winter, Snowy, Soft)", StartTime: t0.Add(24 * time.Hour), EndTime: t0.Add(120 * time.Hour)},
			},
		},
		Announce: mdbdiff.AnnounceDiff{
			Added: []mdbdiff.AnnounceEvent{
				{ID: 30010, AnnounceType: mdbdiff.AnnounceTypeCharacter, Title: "[Beyond the Horizon] Tokai Teio", StartTime: t0.Add(30 * time.Hour), EndTime: t0.Add(200 * time.Hour)},
			},
			Changed: []mdbdiff.EntryChange[mdbdiff.AnnounceEvent]{
				{
					New: mdbdiff.AnnounceEvent{ID: 30005, AnnounceType: mdbdiff.AnnounceTypeSupportCard, Title: "[Fire at My Heels] Kitasan Black", StartTime: t0.Add(10 * time.Hour), EndTime: t0.Add(150 * time.Hour)},
				},
			},
		},
		StoryEvent: mdbdiff.StoryEventDiff{
			Added: []mdbdiff.StoryEvent{
				{ID: 1023, Title: "Leap into a New World!", StartTime: t0.Add(5 * time.Hour), EndTime: t0.Add(300 * time.Hour)},
			},
		},
		Campaign: mdbdiff.CampaignDiff{
			Added: []mdbdiff.CampaignEvent{
				{CampaignID: 50, Title: "Daily Bonus", Description: "Earn 2x rewards in Daily Races!", StartTime: t0.Add(6 * time.Hour), EndTime: t0.Add(72 * time.Hour)},
			},
		},
	}

	msg, ok := FormatResourceUpdateMessage(diff, "role-99")
	if !ok {
		t.Fatal("expected ok=true for non-empty diff")
	}

	// Verify role tag, top heading, section aliases, and section order.
	wantOrder := []string{
		"<@&role-99>",
		"# Game Resource Update Detected!",
		"## Scenario",
		"## Champions Meeting",
		"## Gacha Banner",
		"## Event Story",
		"## Campaign",
	}
	lastPos := -1
	for _, token := range wantOrder {
		idx := strings.Index(msg, token)
		if idx <= lastPos {
			t.Fatalf("token %q missing or out of order (idx=%d, lastPos=%d):\n%s", token, idx, lastPos, msg)
		}
		lastPos = idx
	}

	// Verify Scenario sorted by StartTime (Earlier Scenario ID=20 before Later Scenario ID=10) and omits end time.
	idxEarlier := strings.Index(msg, "**Earlier Scenario**")
	idxLater := strings.Index(msg, "**Later Scenario**")
	if idxEarlier < 0 || idxLater <= idxEarlier {
		t.Errorf("Scenario items not sorted by StartTime ascending:\n%s", msg)
	}
	scenarioSection := strings.Split(msg, "## Champions Meeting")[0]
	if strings.Contains(scenarioSection, "–") {
		t.Errorf("Scenario section must only show start time, not end time:\n%s", scenarioSection)
	}

	// Verify Gacha Banner sorted by StartTime (Support Card ID=30005 at +10h before Character ID=30010 at +30h) and marks (Updated).
	idxSupp := strings.Index(msg, "**[Support Card]** [Fire at My Heels] Kitasan Black (Updated)")
	idxChar := strings.Index(msg, "**[Character]** [Beyond the Horizon] Tokai Teio")
	if idxSupp < 0 || idxChar <= idxSupp {
		t.Errorf("Gacha Banner items missing or not sorted by StartTime ascending:\n%s", msg)
	}

	// Verify CM course summary and Campaign description (category 188).
	if !strings.Contains(msg, "Tokyo Dirt 1600m Left (Winter, Snowy, Soft)") ||
		!strings.Contains(msg, "**Earn 2x rewards in Daily Races!**") {
		t.Errorf("missing expected CM course or Campaign description:\n%s", msg)
	}
}

func TestBroadcastResourceUpdateAndChunking(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "discord.db"))
	if err != nil {
		t.Fatalf("OpenStore() error = %v", err)
	}
	defer store.Close()

	_, _ = store.UpsertResourceGuild(context.Background(), "g1", "c1", "r1")
	_, _ = store.UpsertResourceGuild(context.Background(), "g2", "c2", "")

	now := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	diff := &mdbdiff.DiffResult{
		StoryEvent: mdbdiff.StoryEventDiff{
			Added: []mdbdiff.StoryEvent{
				{ID: 1023, Title: "Leap into a New World!", StartTime: now, EndTime: now.Add(24 * time.Hour)},
			},
		},
	}

	// Fail first guild delivery and verify second guild still receives notification.
	sender := &fakeSender{failOn: 1}
	sent := BroadcastResourceUpdate(context.Background(), store, sender, diff, log.New(io.Discard, "", 0))
	if sent != 1 || len(sender.messages) != 1 || sender.messages[0].channelID != "c2" {
		t.Fatalf("BroadcastResourceUpdate fault isolation unexpected: sent=%d msgs=%+v", sent, sender.messages)
	}

	// Verify SendChannelMessage chunks payloads > 2000 runes and sends allowed_mentions.roles only on first chunk.
	var requests []createMessageRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req createMessageRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Fatalf("unmarshal req: %v", err)
		}
		requests = append(requests, req)
		_, _ = w.Write([]byte(`{"id":"m1"}`))
	}))
	defer srv.Close()

	client, err := NewClient("app-1", "bot-secret", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	var blocks []string
	for i := 0; i < 40; i++ {
		blocks = append(blocks, strings.Repeat("x", 80))
	}
	longContent := strings.Join(blocks, "\n\n")
	if err := client.SendChannelMessage(context.Background(), "c1", longContent, "r1"); err != nil {
		t.Fatalf("SendChannelMessage(long) error = %v", err)
	}
	if len(requests) < 2 {
		t.Fatalf("expected >= 2 chunks for long Discord message, got %d", len(requests))
	}
	for i, req := range requests {
		if len([]rune(req.Content)) > MaxDiscordMessageRunes {
			t.Errorf("chunk %d exceeds %d runes", i, MaxDiscordMessageRunes)
		}
		if i == 0 && (req.AllowedMentions == nil || len(req.AllowedMentions.Roles) != 1 || req.AllowedMentions.Roles[0] != "r1") {
			t.Errorf("chunk 0 missing role mention allowance: %+v", req.AllowedMentions)
		}
		if i > 0 && req.AllowedMentions != nil && len(req.AllowedMentions.Roles) != 0 {
			t.Errorf("chunk %d should not repeat role mentions: %+v", i, req.AllowedMentions)
		}
	}
}

func createChampionsScheduleTestDB(t *testing.T, path string, refNow time.Time) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	defer db.Close()

	// Note: for TestNotifyChannelCommandAdminAndPermissions, refNow = base + 12h so ID 2 (base-1h? Wait: let's define base = refNow - 2h).
	base := refNow.Add(-2 * time.Hour)
	stmts := []string{
		`CREATE TABLE text_data (id INTEGER NOT NULL, category INTEGER NOT NULL, "index" INTEGER NOT NULL, text TEXT NOT NULL, PRIMARY KEY(category, "index"))`,
		`CREATE TABLE champions_race_condition (champions_id INTEGER NOT NULL, round_id INTEGER NOT NULL, race_instance_id INTEGER NOT NULL, race_condition_id INTEGER NOT NULL, PRIMARY KEY(champions_id, round_id))`,
		`CREATE TABLE race_instance (id INTEGER PRIMARY KEY, race_id INTEGER NOT NULL)`,
		`CREATE TABLE race (id INTEGER PRIMARY KEY, course_set INTEGER NOT NULL)`,
		`CREATE TABLE race_course_set (id INTEGER PRIMARY KEY, race_track_id INTEGER NOT NULL, distance INTEGER NOT NULL, ground INTEGER NOT NULL)`,
		`CREATE TABLE champions_round_schedule (id INTEGER PRIMARY KEY, champions_id INTEGER NOT NULL, round INTEGER NOT NULL, round_detail INTEGER NOT NULL, start_date INTEGER NOT NULL, end_date INTEGER NOT NULL, interval_start_time INTEGER NOT NULL, interval_end_time INTEGER NOT NULL)`,
		`INSERT INTO text_data VALUES (206, 206, 23, 'Pisces Cup'), (35, 35, 10009, 'Hanshin')`,
		`INSERT INTO champions_race_condition VALUES (23, 0, 1, 1)`,
		`INSERT INTO race_instance VALUES (1, 101)`,
		`INSERT INTO race VALUES (101, 201)`,
		`INSERT INTO race_course_set VALUES (201, 10009, 2000, 1)`,
		// id=1 (round 0, pre-entry), id=2 (round 1 at base+1h), id=3 (round 2 at base+25h),
		// id=4 (round 3.1 at base+49h), id=5 (round 3.2 matching at base+60h), id=6 (round 3.3 finals at base+73h).
		`INSERT INTO champions_round_schedule VALUES
			(1, 23, 0, 0, ` + itoa(base.Add(-24*time.Hour).Unix()) + `, ` + itoa(base.Add(1*time.Hour).Unix()) + `, 0, 0),
			(2, 23, 1, 0, ` + itoa(base.Add(1*time.Hour).Unix()) + `, ` + itoa(base.Add(25*time.Hour).Unix()) + `, 0, 0),
			(3, 23, 2, 0, ` + itoa(base.Add(25*time.Hour).Unix()) + `, ` + itoa(base.Add(49*time.Hour).Unix()) + `, 0, 0),
			(4, 23, 3, 1, ` + itoa(base.Add(49*time.Hour).Unix()) + `, ` + itoa(base.Add(60*time.Hour).Unix()) + `, 0, 0),
			(5, 23, 3, 2, ` + itoa(base.Add(60*time.Hour).Unix()) + `, ` + itoa(base.Add(73*time.Hour).Unix()) + `, 0, 0),
			(6, 23, 3, 3, ` + itoa(base.Add(73*time.Hour).Unix()) + `, ` + itoa(base.Add(97*time.Hour).Unix()) + `, 0, 0)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("exec %q: %v", s, err)
		}
	}
}

func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}
