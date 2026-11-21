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

	nowUTC := now.UTC()
	var sb strings.Builder
	sb.WriteString("<b>campaign_data changes</b>")

	if len(diff.Added) > 0 {
		sb.WriteString("\n\n<b>[Added]</b>")
		for _, ev := range diff.Added {
			writeCampaignEvent(&sb, ev, nowUTC, true)
		}
	}

	if len(diff.Deleted) > 0 {
		sb.WriteString("\n\n<b>[Deleted]</b>")
		for _, ev := range diff.Deleted {
			writeCampaignEvent(&sb, ev, nowUTC, false)
		}
	}

	if len(diff.Changed) > 0 {
		sb.WriteString("\n\n<b>[Changed]</b>")
		for _, ch := range diff.Changed {
			writeCampaignChange(&sb, ch, nowUTC)
		}
	}

	return sb.String(), true
}

func writeCampaignEvent(sb *strings.Builder, ev mdbdiff.CampaignEvent, nowUTC time.Time, includeCountdown bool) {
	writeCampaignHeader(sb, ev.CampaignID, "", ev.Title, ev.Description)
	startUTC := ev.StartTime.UTC()
	endUTC := ev.EndTime.UTC()
	writeTimeLine(sb, "Start", "Starts", startUTC, startUTC, nowUTC, includeCountdown)
	writeTimeLine(sb, "End", "Ends", endUTC, endUTC, nowUTC, includeCountdown)
}

func writeCampaignChange(sb *strings.Builder, ch mdbdiff.CampaignChange, nowUTC time.Time) {
	writeCampaignHeader(sb, ch.New.CampaignID, ch.Old.Title, ch.New.Title, ch.New.Description)
	writeTimeLine(sb, "Start", "Starts", ch.Old.StartTime.UTC(), ch.New.StartTime.UTC(), nowUTC, true)
	writeTimeLine(sb, "End", "Ends", ch.Old.EndTime.UTC(), ch.New.EndTime.UTC(), nowUTC, true)
}

func writeCampaignHeader(sb *strings.Builder, campaignID int64, oldTitle, newTitle, desc string) {
	title := strings.TrimSpace(newTitle)
	if title == "" {
		title = "(no title)"
	}
	oldTitle = strings.TrimSpace(oldTitle)
	if oldTitle != "" && oldTitle != title {
		fmt.Fprintf(sb, "\n\n<b>#%d %s -> %s</b>",
			campaignID, html.EscapeString(oldTitle), html.EscapeString(title))
	} else {
		fmt.Fprintf(sb, "\n\n<b>#%d %s</b>", campaignID, html.EscapeString(title))
	}

	if trimmedDesc := strings.TrimSpace(desc); trimmedDesc != "" {
		fmt.Fprintf(sb, "\n%s", html.EscapeString(trimmedDesc))
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
