package mdbdiff

import (
	"database/sql"
	"fmt"
	"time"
)

const (
	textCategoryCampaignTitle = 187
	textCategoryCampaignDesc  = 188
)

// CampaignEvent represents a single campaign_data record enriched with its
// title (text_data category 187) and description (text_data category 188).
type CampaignEvent struct {
	CampaignID  int64
	Title       string
	Description string
	EffectType1 int64
	StartTime   time.Time
	EndTime     time.Time
}

// CampaignChange represents a modified campaign_data entry between versions.
type CampaignChange struct {
	Old CampaignEvent
	New CampaignEvent
}

// CampaignDiff contains added, deleted, and changed active campaign_data entries
// (filtered by effect_type_1 != 0).
type CampaignDiff struct {
	Added   []CampaignEvent
	Deleted []CampaignEvent
	Changed []CampaignChange
}

// IsEmpty reports whether there are no added, deleted, or changed campaigns.
func (c CampaignDiff) IsEmpty() bool {
	return len(c.Added) == 0 && len(c.Deleted) == 0 && len(c.Changed) == 0
}

func diffCampaignData(oldDB, newDB *sql.DB) (CampaignDiff, error) {
	oldMap, err := loadActiveCampaigns(oldDB)
	if err != nil {
		return CampaignDiff{}, fmt.Errorf("load old campaigns: %w", err)
	}
	newMap, err := loadActiveCampaigns(newDB)
	if err != nil {
		return CampaignDiff{}, fmt.Errorf("load new campaigns: %w", err)
	}

	added, deleted, changedPairs := diffEntityMaps(oldMap, newMap, func(ev CampaignEvent) int64 {
		return ev.CampaignID
	})
	changed := make([]CampaignChange, len(changedPairs))
	for i, pair := range changedPairs {
		changed[i] = CampaignChange{Old: pair.Old, New: pair.New}
	}
	return CampaignDiff{
		Added:   added,
		Deleted: deleted,
		Changed: changed,
	}, nil
}

func loadActiveCampaigns(db *sql.DB) (map[int64]entityRecord[CampaignEvent], error) {
	texts, err := loadTextLookup(db, textCategoryCampaignTitle, textCategoryCampaignDesc)
	if err != nil {
		return nil, err
	}
	titles := texts[textCategoryCampaignTitle]
	descs := texts[textCategoryCampaignDesc]

	return loadTableEntities(db, "campaign_data",
		`SELECT * FROM campaign_data WHERE effect_type_1 != 0 ORDER BY campaign_id ASC`,
		func(vals []any, colIndex map[string]int) (int64, CampaignEvent, error) {
			campaignID, ok := asInt64(vals[colIndex["campaign_id"]])
			if !ok {
				return 0, CampaignEvent{}, fmt.Errorf("campaign_id has unexpected type %T", vals[colIndex["campaign_id"]])
			}
			effectType1, _ := asInt64(vals[colIndex["effect_type_1"]])
			startUnix, _ := asInt64(vals[colIndex["start_time"]])
			endUnix, _ := asInt64(vals[colIndex["end_time"]])

			return campaignID, CampaignEvent{
				CampaignID:  campaignID,
				Title:       titles[campaignID],
				Description: descs[campaignID],
				EffectType1: effectType1,
				StartTime:   time.Unix(startUnix, 0).UTC(),
				EndTime:     time.Unix(endUnix, 0).UTC(),
			}, nil
		})
}

func asInt64(v any) (int64, bool) {
	n, ok := v.(int64)
	return n, ok
}
