package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kudapan_lokal/internal/mdbdiff"
)

func TestFormatSummaryMessage(t *testing.T) {
	diff := &mdbdiff.DiffResult{
		Tables: []mdbdiff.TableDiff{
			{Table: "campaign_data", Added: 1, Deleted: 2, Changed: 3},
			{Table: "text_data", Added: 4, Deleted: 0, Changed: 1},
			{Table: "card_data", Added: 5, Deleted: 0, Changed: 0},
			{Table: "skill_data", Added: 0, Deleted: 6, Changed: 0},
			{Table: "item_data", Added: 0, Deleted: 0, Changed: 7},
		},
	}

	msg := FormatSummaryMessage("10008320", "10008790", diff)
	for _, want := range []string{
		"campaign_data:\n  1 add(s), 2 del(s), 3 change(s)",
		"text_data:\n  4 add(s), 1 change(s)",
		"card_data:\n  5 add(s)",
		"skill_data:\n  6 del(s)",
		"item_data:\n  7 change(s)",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("summary message missing %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "0 add(s)") || strings.Contains(msg, "0 del(s)") || strings.Contains(msg, "0 change(s)") {
		t.Errorf("summary message must omit zero counts:\n%s", msg)
	}
	if !strings.Contains(msg, "10008320") || !strings.Contains(msg, "10008790") {
		t.Errorf("summary message missing versions:\n%s", msg)
	}

	emptyMsg := FormatSummaryMessage("10008700", "10008790", &mdbdiff.DiffResult{})
	if !strings.Contains(emptyMsg, "No changes in master.mdb.") {
		t.Errorf("expected no-changes note, got:\n%s", emptyMsg)
	}
}

func TestFormatCampaignDiffMessageUTCAndCountdown(t *testing.T) {
	now := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)

	if _, ok := FormatCampaignDiffMessage(mdbdiff.CampaignDiff{}, now); ok {
		t.Fatal("expected empty CampaignDiff to return ok=false")
	}

	diff := mdbdiff.CampaignDiff{
		Added: []mdbdiff.CampaignEvent{
			{
				CampaignID:  301,
				Title:       "Future Event <Test>",
				Description: "Earn 2x rewards!",
				StartTime:   now.Add(2*24*time.Hour + 5*time.Hour),
				EndTime:     now.Add(9 * 24 * time.Hour),
			},
			{
				CampaignID:  302,
				Title:       "Active Event",
				Description: "Already running",
				StartTime:   now.Add(-3 * time.Hour),
				EndTime:     now.Add(48 * time.Hour),
			},
			{
				CampaignID:  304,
				Title:       "Ended Event",
				Description: "Already finished",
				StartTime:   now.Add(-72 * time.Hour),
				EndTime:     now.Add(-1 * time.Hour),
			},
		},
		Changed: []mdbdiff.CampaignChange{
			{
				Old: mdbdiff.CampaignEvent{
					CampaignID:  303,
					Title:       "Extended Event",
					Description: "Extended",
					StartTime:   now.Add(30 * time.Minute),
					EndTime:     now.Add(72 * time.Hour),
				},
				New: mdbdiff.CampaignEvent{
					CampaignID:  303,
					Title:       "Extended Event",
					Description: "Extended",
					StartTime:   now.Add(30 * time.Minute),
					EndTime:     now.Add(96 * time.Hour),
				},
			},
		},
	}

	msg, ok := FormatCampaignDiffMessage(diff, now)
	if !ok {
		t.Fatal("expected non-empty campaign diff message")
	}

	// [Deleted] must be omitted when there are no deleted rows.
	if strings.Contains(msg, "[Deleted]") {
		t.Errorf("unexpected [Deleted] section:\n%s", msg)
	}
	if !strings.Contains(msg, "[Added]") || !strings.Contains(msg, "[Changed]") {
		t.Errorf("missing expected [Added]/[Changed] sections:\n%s", msg)
	}
	if !strings.Contains(msg, "Future Event &lt;Test&gt;") {
		t.Errorf("expected HTML-escaped title:\n%s", msg)
	}
	if !strings.Contains(msg, "Starts: 2026-11-23 05:00:00 UTC (in 2 days 5 hours)") {
		t.Errorf("expected inline start countdown:\n%s", msg)
	}
	if !strings.Contains(msg, "Ends: 2026-11-30 00:00:00 UTC (in 9 days 0 hours)") {
		t.Errorf("expected inline end countdown:\n%s", msg)
	}
	if !strings.Contains(msg, "Starts: 2026-11-21 00:30:00 UTC (in less than 1 hour)") {
		t.Errorf("expected inline <1h start countdown:\n%s", msg)
	}
	if !strings.Contains(msg, "Ends: 2026-11-24 00:00:00 UTC -> 2026-11-25 00:00:00 UTC (in 4 days 0 hours)") {
		t.Errorf("expected changed end line with inline countdown:\n%s", msg)
	}
	if strings.Contains(msg, "Starts in:") {
		t.Errorf("legacy standalone 'Starts in:' line should be removed:\n%s", msg)
	}

	// Verify Active Event (started 3h ago, ends in 2 days) omits start countdown but includes end countdown.
	activeSection := strings.Split(msg, "#302 Active Event")[1]
	activeSection = strings.Split(activeSection, "#304 Ended Event")[0]
	if !strings.Contains(activeSection, "Start: 2026-11-20 21:00:00 UTC\n") {
		t.Errorf("already-started campaign should use plain 'Start:' without countdown:\n%s", activeSection)
	}
	if !strings.Contains(activeSection, "Ends: 2026-11-23 00:00:00 UTC (in 2 days 0 hours)") {
		t.Errorf("active campaign should include inline end countdown:\n%s", activeSection)
	}

	// Verify Ended Event (ended 1h ago) omits both start and end countdowns.
	endedSection := strings.Split(msg, "#304 Ended Event")[1]
	endedSection = strings.Split(endedSection, "[Changed]")[0]
	if !strings.Contains(endedSection, "Start: 2026-11-18 00:00:00 UTC\nEnd: 2026-11-20 23:00:00 UTC") {
		t.Errorf("ended campaign should omit both countdowns:\n%s", endedSection)
	}
}

func TestFormatScenarioAndChampionsMessage(t *testing.T) {
	now := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)

	if msg, ok := FormatScenarioAndChampionsMessage(mdbdiff.ScenarioDiff{}, mdbdiff.ChampionsDiff{}, now); ok || msg != "" {
		t.Fatalf("expected empty diff to return (\"\", false), got (%q, %v)", msg, ok)
	}

	scenarioOnly := mdbdiff.ScenarioDiff{
		Added: []mdbdiff.ScenarioEvent{
			{
				ID:         5,
				Title:      "Grandmasters Legacies Immortal",
				StartTime:  now.Add(48*time.Hour + 3*time.Hour),
				EndTime:    now.Add(10 * 24 * time.Hour),
				MaxSpeed:   300,
				MaxStamina: 300,
				MaxPow:     300,
				MaxGuts:    300,
				MaxWiz:     400,
			},
		},
	}
	msgScenarioOnly, ok := FormatScenarioAndChampionsMessage(scenarioOnly, mdbdiff.ChampionsDiff{}, now)
	if !ok {
		t.Fatal("expected ok=true when single_mode_scenario has diffs")
	}
	if !strings.Contains(msgScenarioOnly, "single_mode_scenario changes") || strings.Contains(msgScenarioOnly, "champions_schedule changes") {
		t.Errorf("unexpected section headings:\n%s", msgScenarioOnly)
	}
	if !strings.Contains(msgScenarioOnly, "Max stat bonus: Spd +300 / Sta +300 / Pow +300 / Gut +300 / Wit +400") ||
		!strings.Contains(msgScenarioOnly, "Starts: 2026-11-24 03:00:00 UTC (in 2 days 3 hours)") {
		t.Errorf("unexpected scenario-only body:\n%s", msgScenarioOnly)
	}

	championsOnly := mdbdiff.ChampionsDiff{
		Added: []mdbdiff.ChampionsScheduleEvent{
			{
				ID:        23,
				Title:     "Pisces Cup",
				Course:    "Hanshin Turf 2000m",
				StartTime: now.Add(5 * 24 * time.Hour),
				EndTime:   now.Add(11 * 24 * time.Hour),
			},
		},
		Changed: []mdbdiff.EntryChange[mdbdiff.ChampionsScheduleEvent]{
			{
				Old: mdbdiff.ChampionsScheduleEvent{
					ID:        22,
					Title:     "Aquarius Cup",
					Course:    "Tokyo Dirt 1600m",
					StartTime: now.Add(-24 * time.Hour),
					EndTime:   now.Add(72 * time.Hour),
				},
				New: mdbdiff.ChampionsScheduleEvent{
					ID:        22,
					Title:     "Aquarius Cup",
					Course:    "Tokyo Dirt 1600m",
					StartTime: now.Add(-24 * time.Hour),
					EndTime:   now.Add(96 * time.Hour),
				},
			},
		},
	}
	msgChampOnly, ok := FormatScenarioAndChampionsMessage(mdbdiff.ScenarioDiff{}, championsOnly, now)
	if !ok {
		t.Fatal("expected ok=true when champions_schedule has diffs")
	}
	if strings.Contains(msgChampOnly, "single_mode_scenario changes") || !strings.Contains(msgChampOnly, "champions_schedule changes") {
		t.Errorf("unexpected section headings:\n%s", msgChampOnly)
	}
	if !strings.Contains(msgChampOnly, "Course: Hanshin Turf 2000m") ||
		!strings.Contains(msgChampOnly, "Ends: 2026-11-25 00:00:00 UTC -> 2026-11-26 00:00:00 UTC (in 4 days 0 hours)") {
		t.Errorf("unexpected champions-only body:\n%s", msgChampOnly)
	}

	msgBoth, ok := FormatScenarioAndChampionsMessage(scenarioOnly, championsOnly, now)
	if !ok || !strings.Contains(msgBoth, "single_mode_scenario changes") || !strings.Contains(msgBoth, "champions_schedule changes") {
		t.Errorf("expected both sections when both differ:\n%s", msgBoth)
	}

	storyOnly := mdbdiff.StoryEventDiff{
		Added: []mdbdiff.StoryEvent{
			{
				ID:        1023,
				Title:     "Leap into a New World!",
				StartTime: now.Add(3 * 24 * time.Hour),
				EndTime:   now.Add(15 * 24 * time.Hour),
			},
		},
	}
	msgAllThree, ok := FormatScheduleMessage(scenarioOnly, championsOnly, storyOnly, now)
	if !ok {
		t.Fatal("expected ok=true when all three sections differ")
	}
	idxScenario := strings.Index(msgAllThree, "single_mode_scenario changes")
	idxChampions := strings.Index(msgAllThree, "champions_schedule changes")
	idxStory := strings.Index(msgAllThree, "story_event_data changes")
	if idxScenario < 0 || idxChampions <= idxScenario || idxStory <= idxChampions {
		t.Errorf("unexpected section order (scenario=%d, champions=%d, story=%d):\n%s",
			idxScenario, idxChampions, idxStory, msgAllThree)
	}
	if !strings.Contains(msgAllThree, "#1023 Leap into a New World!") {
		t.Errorf("missing story_event_data title:\n%s", msgAllThree)
	}
}

func TestFormatCampaignAndAnnounceMessage(t *testing.T) {
	now := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)

	if msg, ok := FormatCampaignAndAnnounceMessage(mdbdiff.CampaignDiff{}, mdbdiff.AnnounceDiff{}, now); ok || msg != "" {
		t.Fatalf("expected empty diff to return (\"\", false), got (%q, %v)", msg, ok)
	}

	announce := mdbdiff.AnnounceDiff{
		Added: []mdbdiff.AnnounceEvent{
			{
				ID:           30010,
				AnnounceType: mdbdiff.AnnounceTypeCharacter,
				AnnounceID:   104,
				Title:        "[Beyond the Horizon] Tokai Teio, [End of the Skies] Mejiro McQueen",
				StartTime:    now.Add(24 * time.Hour),
				EndTime:      now.Add(10 * 24 * time.Hour),
			},
			{
				ID:           30011,
				AnnounceType: mdbdiff.AnnounceTypeSupportCard,
				AnnounceID:   504,
				Title:        "[Fire at My Heels] Kitasan Black, [The Will to Overtake] Satono Diamond",
				StartTime:    now.Add(24 * time.Hour),
				EndTime:      now.Add(10 * 24 * time.Hour),
			},
		},
	}

	msgAnnounceOnly, ok := FormatCampaignAndAnnounceMessage(mdbdiff.CampaignDiff{}, announce, now)
	if !ok {
		t.Fatal("expected ok=true for announce-only diff")
	}
	if strings.Contains(msgAnnounceOnly, "campaign_data changes") || !strings.Contains(msgAnnounceOnly, "announce_data changes") {
		t.Errorf("unexpected headings:\n%s", msgAnnounceOnly)
	}
	if !strings.Contains(msgAnnounceOnly, "#30010 [Beyond the Horizon] Tokai Teio, [End of the Skies] Mejiro McQueen") ||
		!strings.Contains(msgAnnounceOnly, "Type: Character") ||
		!strings.Contains(msgAnnounceOnly, "Type: Support Card") ||
		!strings.Contains(msgAnnounceOnly, "Starts: 2026-11-23 00:00:00 UTC (in 1 day 0 hours)") {
		t.Errorf("unexpected announce-only body:\n%s", msgAnnounceOnly)
	}

	campaign := mdbdiff.CampaignDiff{
		Added: []mdbdiff.CampaignEvent{
			{
				CampaignID:  10,
				Title:       "Double Drops",
				Description: "2x rewards",
				StartTime:   now.Add(2 * time.Hour),
				EndTime:     now.Add(48 * time.Hour),
			},
		},
	}
	msgBoth, ok := FormatCampaignAndAnnounceMessage(campaign, announce, now)
	if !ok || !strings.Contains(msgBoth, "campaign_data changes") || !strings.Contains(msgBoth, "announce_data changes") {
		t.Errorf("expected both campaign_data and announce_data sections:\n%s", msgBoth)
	}
}

func TestSplitMessageAndTelegramSend(t *testing.T) {
	var lines []string
	for i := 0; i < 120; i++ {
		lines = append(lines, strings.Repeat("a", 50))
	}
	longText := strings.Join(lines, "\n")
	chunks := SplitMessage(longText, 4096)
	if len(chunks) < 2 {
		t.Fatalf("expected >= 2 chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if len(c) > 4096 {
			t.Fatalf("chunk %d length %d > 4096", i, len(c))
		}
	}

	var sent []sendMessageRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/botsecret-token/sendMessage" {
			t.Errorf("path = %s", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		var req sendMessageRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Fatalf("unmarshal req: %v", err)
		}
		sent = append(sent, req)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	client, err := NewTelegramClient("secret-token", "12345", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatalf("NewTelegramClient() error = %v", err)
	}
	if err := client.SendMessage(context.Background(), longText); err != nil {
		t.Fatalf("SendMessage() error = %v", err)
	}
	if len(sent) != len(chunks) {
		t.Fatalf("sent %d requests, want %d", len(sent), len(chunks))
	}
	if sent[0].ChatID != "12345" || sent[0].ParseMode != "HTML" {
		t.Errorf("unexpected request metadata: %+v", sent[0])
	}

	// Verify API error propagation.
	errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`))
	}))
	defer errSrv.Close()

	errClient, _ := NewTelegramClient("secret-token", "0", WithBaseURL(errSrv.URL), WithHTTPClient(errSrv.Client()))
	if err := errClient.SendMessage(context.Background(), "hello"); err == nil {
		t.Fatal("expected error when Telegram returns ok=false")
	}
}

func TestTelegramSendMessageToChat(t *testing.T) {
	var got sendMessageRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("unmarshal req: %v", err)
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	client, err := NewTelegramClient("secret-token", "default-chat", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatalf("NewTelegramClient() error = %v", err)
	}

	if err := client.SendMessageToChat(context.Background(), "   ", "hello"); err == nil {
		t.Fatal("expected error for empty target chatID")
	}

	if err := client.SendMessageToChat(context.Background(), " 999888 ", "Pong!"); err != nil {
		t.Fatalf("SendMessageToChat() error = %v", err)
	}
	if got.ChatID != "999888" || got.Text != "Pong!" || got.ParseMode != "HTML" {
		t.Errorf("unexpected request payload: %+v", got)
	}
}
