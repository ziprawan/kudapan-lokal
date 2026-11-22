package notify

import (
	"fmt"
	"html"
	"strings"
	"time"

	"kudapan_lokal/internal/mdbdiff"
)

const utcTimeLayout = "2006-01-02 15:04:05 UTC"

// FormatSummaryMessage formats Message 1: resource version header plus non-zero
// per-table add/del/change counts.
func FormatSummaryMessage(oldResVer, newResVer string, diff *mdbdiff.DiffResult) string {
	var sb strings.Builder

	if oldResVer != "" {
		fmt.Fprintf(&sb, "<b>master.mdb updated</b> (<code>%s</code> -> <code>%s</code>)\n",
			html.EscapeString(oldResVer), html.EscapeString(newResVer))
	} else {
		fmt.Fprintf(&sb, "<b>master.mdb updated</b> (<code>%s</code>)\n", html.EscapeString(newResVer))
	}

	var body strings.Builder
	if diff != nil {
		for _, td := range diff.Tables {
			parts := formatTableDiffParts(td)
			if len(parts) == 0 {
				continue
			}
			fmt.Fprintf(&body, "\n%s:\n  %s",
				html.EscapeString(td.Table), strings.Join(parts, ", "))
		}
	}
	if body.Len() == 0 {
		sb.WriteString("\nNo changes in master.mdb.")
		return sb.String()
	}

	sb.WriteString(body.String())
	return sb.String()
}

func formatTableDiffParts(td mdbdiff.TableDiff) []string {
	var parts []string
	if td.Added > 0 {
		parts = append(parts, fmt.Sprintf("%d add(s)", td.Added))
	}
	if td.Deleted > 0 {
		parts = append(parts, fmt.Sprintf("%d del(s)", td.Deleted))
	}
	if td.Changed > 0 {
		parts = append(parts, fmt.Sprintf("%d change(s)", td.Changed))
	}
	return parts
}

// FormatCampaignDiffMessage formats Message 2: detailed campaign_data diffs
// (filtered to effect_type_1 != 0) enriched with text_data titles/descriptions,
// UTC timestamps, and inline countdowns for upcoming starts and active ends.
// Returns ("", false) when empty.
func FormatCampaignDiffMessage(diff mdbdiff.CampaignDiff, now time.Time) (string, bool) {
	if diff.IsEmpty() {
		return "", false
	}

	changed := make([]mdbdiff.EntryChange[mdbdiff.CampaignEvent], len(diff.Changed))
	for i, ch := range diff.Changed {
		changed[i] = mdbdiff.EntryChange[mdbdiff.CampaignEvent]{Old: ch.Old, New: ch.New}
	}
	entryDiff := mdbdiff.EntryDiff[mdbdiff.CampaignEvent]{
		Added:   diff.Added,
		Deleted: diff.Deleted,
		Changed: changed,
	}

	var sb strings.Builder
	writeEntryDiffBlock(&sb, "campaign_data changes", entryDiff, now.UTC(), formatCampaignItem, formatCampaignChangeItem)
	return sb.String(), true
}

// FormatScenarioAndChampionsMessage formats Message 3: detailed diffs for
// single_mode_scenario and champions_schedule. Returns ("", false) when both
// tables have no differences.
func FormatScenarioAndChampionsMessage(
	scenario mdbdiff.ScenarioDiff,
	champions mdbdiff.ChampionsDiff,
	now time.Time,
) (string, bool) {
	if scenario.IsEmpty() && champions.IsEmpty() {
		return "", false
	}

	nowUTC := now.UTC()
	var sb strings.Builder
	wroteSection := false

	if !scenario.IsEmpty() {
		writeEntryDiffBlock(&sb, "single_mode_scenario changes", scenario, nowUTC, formatScenarioItem, formatScenarioChangeItem)
		wroteSection = true
	}
	if !champions.IsEmpty() {
		if wroteSection {
			sb.WriteString("\n\n")
		}
		writeEntryDiffBlock(&sb, "champions_schedule changes", champions, nowUTC, formatChampionsItem, formatChampionsChangeItem)
	}

	return sb.String(), true
}

func writeEntryDiffBlock[T any](
	sb *strings.Builder,
	heading string,
	diff mdbdiff.EntryDiff[T],
	nowUTC time.Time,
	writeItem func(sb *strings.Builder, item T, nowUTC time.Time, includeCountdown bool),
	writeChanged func(sb *strings.Builder, ch mdbdiff.EntryChange[T], nowUTC time.Time),
) {
	fmt.Fprintf(sb, "<b>%s</b>", html.EscapeString(heading))

	if len(diff.Added) > 0 {
		sb.WriteString("\n\n<b>[Added]</b>")
		for _, item := range diff.Added {
			writeItem(sb, item, nowUTC, true)
		}
	}
	if len(diff.Deleted) > 0 {
		sb.WriteString("\n\n<b>[Deleted]</b>")
		for _, item := range diff.Deleted {
			writeItem(sb, item, nowUTC, false)
		}
	}
	if len(diff.Changed) > 0 {
		sb.WriteString("\n\n<b>[Changed]</b>")
		for _, ch := range diff.Changed {
			writeChanged(sb, ch, nowUTC)
		}
	}
}

func formatCampaignItem(sb *strings.Builder, ev mdbdiff.CampaignEvent, nowUTC time.Time, includeCountdown bool) {
	writeEntityHeader(sb, ev.CampaignID, "", ev.Title, ev.Description)
	startUTC := ev.StartTime.UTC()
	endUTC := ev.EndTime.UTC()
	writeTimeLine(sb, "Start", "Starts", startUTC, startUTC, nowUTC, includeCountdown)
	writeTimeLine(sb, "End", "Ends", endUTC, endUTC, nowUTC, includeCountdown)
}

func formatCampaignChangeItem(sb *strings.Builder, ch mdbdiff.EntryChange[mdbdiff.CampaignEvent], nowUTC time.Time) {
	writeEntityHeader(sb, ch.New.CampaignID, ch.Old.Title, ch.New.Title, ch.New.Description)
	writeTimeLine(sb, "Start", "Starts", ch.Old.StartTime.UTC(), ch.New.StartTime.UTC(), nowUTC, true)
	writeTimeLine(sb, "End", "Ends", ch.Old.EndTime.UTC(), ch.New.EndTime.UTC(), nowUTC, true)
}

func formatScenarioItem(sb *strings.Builder, ev mdbdiff.ScenarioEvent, nowUTC time.Time, includeCountdown bool) {
	var extra string
	if ev.HasStatBonuses() {
		extra = formatScenarioBonuses(ev)
	}
	writeEntityHeader(sb, ev.ID, "", ev.Title, extra)
	startUTC := ev.StartTime.UTC()
	endUTC := ev.EndTime.UTC()
	writeTimeLine(sb, "Start", "Starts", startUTC, startUTC, nowUTC, includeCountdown)
	writeTimeLine(sb, "End", "Ends", endUTC, endUTC, nowUTC, includeCountdown)
}

func formatScenarioChangeItem(sb *strings.Builder, ch mdbdiff.EntryChange[mdbdiff.ScenarioEvent], nowUTC time.Time) {
	var extra string
	oldBonus := formatScenarioBonuses(ch.Old)
	newBonus := formatScenarioBonuses(ch.New)
	switch {
	case ch.Old.HasStatBonuses() && ch.New.HasStatBonuses() && oldBonus != newBonus:
		extra = fmt.Sprintf("%s -> %s", oldBonus, newBonus)
	case ch.New.HasStatBonuses():
		extra = newBonus
	}
	writeEntityHeader(sb, ch.New.ID, ch.Old.Title, ch.New.Title, extra)
	writeTimeLine(sb, "Start", "Starts", ch.Old.StartTime.UTC(), ch.New.StartTime.UTC(), nowUTC, true)
	writeTimeLine(sb, "End", "Ends", ch.Old.EndTime.UTC(), ch.New.EndTime.UTC(), nowUTC, true)
}

func formatScenarioBonuses(ev mdbdiff.ScenarioEvent) string {
	return fmt.Sprintf("Max stat bonus: Spd +%d / Sta +%d / Pow +%d / Gut +%d / Wit +%d",
		ev.MaxSpeed, ev.MaxStamina, ev.MaxPow, ev.MaxGuts, ev.MaxWiz)
}

func formatChampionsItem(sb *strings.Builder, ev mdbdiff.ChampionsScheduleEvent, nowUTC time.Time, includeCountdown bool) {
	var courseLine string
	if ev.Course != "" {
		courseLine = "Course: " + ev.Course
	}
	writeEntityHeader(sb, ev.ID, "", ev.Title, courseLine)
	startUTC := ev.StartTime.UTC()
	endUTC := ev.EndTime.UTC()
	writeTimeLine(sb, "Start", "Starts", startUTC, startUTC, nowUTC, includeCountdown)
	writeTimeLine(sb, "End", "Ends", endUTC, endUTC, nowUTC, includeCountdown)
}

func formatChampionsChangeItem(sb *strings.Builder, ch mdbdiff.EntryChange[mdbdiff.ChampionsScheduleEvent], nowUTC time.Time) {
	var courseLine string
	switch {
	case ch.Old.Course != "" && ch.New.Course != "" && ch.Old.Course != ch.New.Course:
		courseLine = fmt.Sprintf("Course: %s -> %s", ch.Old.Course, ch.New.Course)
	case ch.New.Course != "":
		courseLine = "Course: " + ch.New.Course
	}
	writeEntityHeader(sb, ch.New.ID, ch.Old.Title, ch.New.Title, courseLine)
	writeTimeLine(sb, "Start", "Starts", ch.Old.StartTime.UTC(), ch.New.StartTime.UTC(), nowUTC, true)
	writeTimeLine(sb, "End", "Ends", ch.Old.EndTime.UTC(), ch.New.EndTime.UTC(), nowUTC, true)
}

func writeEntityHeader(sb *strings.Builder, id int64, oldTitle, newTitle, subline string) {
	title := strings.TrimSpace(newTitle)
	if title == "" {
		title = "(no title)"
	}
	oldTitle = strings.TrimSpace(oldTitle)
	if oldTitle != "" && oldTitle != title {
		fmt.Fprintf(sb, "\n\n<b>#%d %s -> %s</b>",
			id, html.EscapeString(oldTitle), html.EscapeString(title))
	} else {
		fmt.Fprintf(sb, "\n\n<b>#%d %s</b>", id, html.EscapeString(title))
	}

	if trimmed := strings.TrimSpace(subline); trimmed != "" {
		fmt.Fprintf(sb, "\n%s", html.EscapeString(trimmed))
	}
}

func writeTimeLine(sb *strings.Builder, pastLabel, futureLabel string, oldT, newT, nowUTC time.Time, includeCountdown bool) {
	showCountdown := includeCountdown && newT.After(nowUTC)
	label := pastLabel
	if showCountdown {
		label = futureLabel
	}

	if !oldT.Equal(newT) {
		fmt.Fprintf(sb, "\n%s: %s -> %s", label, oldT.Format(utcTimeLayout), newT.Format(utcTimeLayout))
	} else {
		fmt.Fprintf(sb, "\n%s: %s", label, newT.Format(utcTimeLayout))
	}
	if showCountdown {
		fmt.Fprintf(sb, " (in %s)", FormatCountdown(newT.Sub(nowUTC)))
	}
}

// FormatCountdown formats a positive duration into days and hours (e.g. "2 days 5 hours").
func FormatCountdown(d time.Duration) string {
	if d <= 0 {
		return "0 hours"
	}
	totalHours := int(d / time.Hour)
	if totalHours == 0 {
		return "less than 1 hour"
	}
	days := totalHours / 24
	hours := totalHours % 24
	if days == 0 {
		return pluralUnit(hours, "hour")
	}
	return fmt.Sprintf("%s %s", pluralUnit(days, "day"), pluralUnit(hours, "hour"))
}

func pluralUnit(n int, unit string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", unit)
	}
	return fmt.Sprintf("%d %ss", n, unit)
}
