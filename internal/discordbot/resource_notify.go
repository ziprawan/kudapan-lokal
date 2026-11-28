package discordbot

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"kudapan_lokal/internal/mdbdiff"
)

type resourceItem[T any] struct {
	item    T
	updated bool
}

// FormatResourceUpdateMessage formats a combined Discord resource update
// notification covering Scenario, Champions Meeting, Gacha Banner, Event Story,
// and Campaign additions and updates (sorted by StartTime ascending within each
// section). Returns ("", false) when all five sections are empty.
func FormatResourceUpdateMessage(diff *mdbdiff.DiffResult, roleID string) (string, bool) {
	if diff == nil {
		return "", false
	}

	var sections []string

	if items := combineAndSort(diff.Scenario.Added, diff.Scenario.Changed,
		func(e mdbdiff.ScenarioEvent) time.Time { return e.StartTime },
		func(e mdbdiff.ScenarioEvent) int64 { return e.ID },
	); len(items) > 0 {
		sections = append(sections, formatSection("Scenario", items, func(entry resourceItem[mdbdiff.ScenarioEvent]) string {
			ev := entry.item
			title := fallbackTitle(ev.Title, "Scenario", ev.ID)
			startUnix := ev.StartTime.UTC().Unix()
			return fmt.Sprintf("**%s%s** (<t:%d:R>)\n-# Starts <t:%d:f>",
				title, updatedSuffix(entry.updated), startUnix, startUnix)
		}))
	}

	if items := combineAndSort(diff.Champions.Added, diff.Champions.Changed,
		func(e mdbdiff.ChampionsScheduleEvent) time.Time { return e.StartTime },
		func(e mdbdiff.ChampionsScheduleEvent) int64 { return e.ID },
	); len(items) > 0 {
		sections = append(sections, formatSection("Champions Meeting", items, func(entry resourceItem[mdbdiff.ChampionsScheduleEvent]) string {
			ev := entry.item
			title := fallbackTitle(ev.Title, "Champions Meeting", ev.ID)
			startUnix := ev.StartTime.UTC().Unix()
			endUnix := ev.EndTime.UTC().Unix()
			if course := normalizeSingleLine(ev.Course); course != "" {
				return fmt.Sprintf("**%s%s** (<t:%d:R>)\n%s\n-# <t:%d:f> – <t:%d:f>",
					title, updatedSuffix(entry.updated), startUnix, course, startUnix, endUnix)
			}
			return fmt.Sprintf("**%s%s** (<t:%d:R>)\n-# <t:%d:f> – <t:%d:f>",
				title, updatedSuffix(entry.updated), startUnix, startUnix, endUnix)
		}))
	}

	if items := combineAndSort(diff.Announce.Added, diff.Announce.Changed,
		func(e mdbdiff.AnnounceEvent) time.Time { return e.StartTime },
		func(e mdbdiff.AnnounceEvent) int64 { return e.ID },
	); len(items) > 0 {
		sections = append(sections, formatSection("Gacha Banner", items, func(entry resourceItem[mdbdiff.AnnounceEvent]) string {
			ev := entry.item
			title := fallbackTitle(ev.Title, "Banner", ev.ID)
			startUnix := ev.StartTime.UTC().Unix()
			endUnix := ev.EndTime.UTC().Unix()
			return fmt.Sprintf("**[%s]** %s%s (<t:%d:R>)\n-# <t:%d:f> – <t:%d:f>",
				ev.TypeLabel(), title, updatedSuffix(entry.updated), startUnix, startUnix, endUnix)
		}))
	}

	if items := combineAndSort(diff.StoryEvent.Added, diff.StoryEvent.Changed,
		func(e mdbdiff.StoryEvent) time.Time { return e.StartTime },
		func(e mdbdiff.StoryEvent) int64 { return e.ID },
	); len(items) > 0 {
		sections = append(sections, formatSection("Event Story", items, func(entry resourceItem[mdbdiff.StoryEvent]) string {
			ev := entry.item
			title := fallbackTitle(ev.Title, "Event Story", ev.ID)
			startUnix := ev.StartTime.UTC().Unix()
			endUnix := ev.EndTime.UTC().Unix()
			return fmt.Sprintf("**%s%s** (<t:%d:R>)\n-# <t:%d:f> – <t:%d:f>",
				title, updatedSuffix(entry.updated), startUnix, startUnix, endUnix)
		}))
	}

	campaignChanged := make([]mdbdiff.EntryChange[mdbdiff.CampaignEvent], len(diff.Campaign.Changed))
	for i, ch := range diff.Campaign.Changed {
		campaignChanged[i] = mdbdiff.EntryChange[mdbdiff.CampaignEvent]{Old: ch.Old, New: ch.New}
	}
	if items := combineAndSort(diff.Campaign.Added, campaignChanged,
		func(e mdbdiff.CampaignEvent) time.Time { return e.StartTime },
		func(e mdbdiff.CampaignEvent) int64 { return e.CampaignID },
	); len(items) > 0 {
		sections = append(sections, formatSection("Campaign", items, func(entry resourceItem[mdbdiff.CampaignEvent]) string {
			ev := entry.item
			text := normalizeSingleLine(ev.Description)
			if text == "" {
				text = normalizeSingleLine(ev.Title)
			}
			if text == "" {
				text = fmt.Sprintf("Campaign #%d", ev.CampaignID)
			}
			startUnix := ev.StartTime.UTC().Unix()
			endUnix := ev.EndTime.UTC().Unix()
			return fmt.Sprintf("**%s%s** (<t:%d:R>)\n-# <t:%d:f> – <t:%d:f>",
				text, updatedSuffix(entry.updated), startUnix, startUnix, endUnix)
		}))
	}

	if len(sections) == 0 {
		return "", false
	}

	var sb strings.Builder
	if roleID := strings.TrimSpace(roleID); roleID != "" {
		fmt.Fprintf(&sb, "<@&%s>\n", roleID)
	}
	sb.WriteString("# Game Resource Update Detected!\n\n")
	sb.WriteString(strings.Join(sections, "\n\n"))
	return sb.String(), true
}

// BroadcastResourceUpdate sends the simplified game resource update message to
// all Discord guilds registered via /notify-resource. Per-guild delivery errors
// are logged and do not abort remaining guilds or fail the primary watcher.
func BroadcastResourceUpdate(
	ctx context.Context,
	store *Store,
	sender MessageSender,
	diff *mdbdiff.DiffResult,
	logger *log.Logger,
) int {
	if store == nil || sender == nil {
		return 0
	}
	if logger == nil {
		logger = log.Default()
	}
	if _, ok := FormatResourceUpdateMessage(diff, ""); !ok {
		return 0
	}

	guilds, err := store.ListResourceGuilds(ctx)
	if err != nil {
		logger.Printf("[discord-resource] list resource guilds failed: %v", err)
		return 0
	}
	if len(guilds) == 0 {
		return 0
	}

	sent := 0
	for _, g := range guilds {
		msg, ok := FormatResourceUpdateMessage(diff, g.RoleID)
		if !ok {
			continue
		}
		if err := sender.SendChannelMessage(ctx, g.ChannelID, msg, g.RoleID); err != nil {
			logger.Printf("[discord-resource] failed to notify guild %q channel %q: %v", g.GuildID, g.ChannelID, err)
			continue
		}
		sent++
	}
	logger.Printf("[discord-resource] sent resource update notification to %d/%d guild(s)", sent, len(guilds))
	return sent
}

func combineAndSort[T any](
	added []T,
	changed []mdbdiff.EntryChange[T],
	startOf func(T) time.Time,
	idOf func(T) int64,
) []resourceItem[T] {
	if len(added) == 0 && len(changed) == 0 {
		return nil
	}
	out := make([]resourceItem[T], 0, len(added)+len(changed))
	for _, item := range added {
		out = append(out, resourceItem[T]{item: item})
	}
	for _, ch := range changed {
		out = append(out, resourceItem[T]{item: ch.New, updated: true})
	}
	sort.Slice(out, func(i, j int) bool {
		si, sj := startOf(out[i].item), startOf(out[j].item)
		if !si.Equal(sj) {
			return si.Before(sj)
		}
		return idOf(out[i].item) < idOf(out[j].item)
	})
	return out
}

func formatSection[T any](heading string, items []resourceItem[T], render func(resourceItem[T]) string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "## %s", heading)
	for _, item := range items {
		sb.WriteString("\n")
		sb.WriteString(render(item))
	}
	return sb.String()
}

func updatedSuffix(updated bool) string {
	if updated {
		return " (Updated)"
	}
	return ""
}

func fallbackTitle(raw, prefix string, id int64) string {
	if s := normalizeSingleLine(raw); s != "" {
		return s
	}
	return fmt.Sprintf("%s #%d", prefix, id)
}

func normalizeSingleLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}
