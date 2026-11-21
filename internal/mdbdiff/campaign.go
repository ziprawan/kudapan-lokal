package mdbdiff

import (
	"database/sql"
	"fmt"
	"sort"
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

type campaignRecord struct {
	event  CampaignEvent
	rowKey string
}

func diffCampaignData(oldDB, newDB *sql.DB) (CampaignDiff, error) {
	var out CampaignDiff

	oldMap, err := loadActiveCampaigns(oldDB)
	if err != nil {
		return out, fmt.Errorf("load old campaigns: %w", err)
	}
	newMap, err := loadActiveCampaigns(newDB)
	if err != nil {
		return out, fmt.Errorf("load new campaigns: %w", err)
	}

	for id, newRec := range newMap {
		oldRec, ok := oldMap[id]
		if !ok {
			out.Added = append(out.Added, newRec.event)
			continue
		}
		if oldRec.rowKey != newRec.rowKey {
			out.Changed = append(out.Changed, CampaignChange{
				Old: oldRec.event,
				New: newRec.event,
			})
		}
	}
	for id, oldRec := range oldMap {
		if _, ok := newMap[id]; !ok {
			out.Deleted = append(out.Deleted, oldRec.event)
		}
	}

	sort.Slice(out.Added, func(i, j int) bool {
		return out.Added[i].CampaignID < out.Added[j].CampaignID
	})
	sort.Slice(out.Deleted, func(i, j int) bool {
		return out.Deleted[i].CampaignID < out.Deleted[j].CampaignID
	})
	sort.Slice(out.Changed, func(i, j int) bool {
		return out.Changed[i].New.CampaignID < out.Changed[j].New.CampaignID
	})

	return out, nil
}

func loadActiveCampaigns(db *sql.DB) (map[int64]campaignRecord, error) {
	hasCampaign, err := tableExists(db, "campaign_data")
	if err != nil {
		return nil, err
	}
	if !hasCampaign {
		return map[int64]campaignRecord{}, nil
	}

	titles := make(map[int64]string)
	descs := make(map[int64]string)
	hasText, err := tableExists(db, "text_data")
	if err != nil {
		return nil, err
	}
	if hasText {
		tRows, err := db.Query(`SELECT category, "index", text FROM text_data WHERE category IN (?, ?)`,
			textCategoryCampaignTitle, textCategoryCampaignDesc)
		if err != nil {
			return nil, fmt.Errorf("query text_data: %w", err)
		}
		defer tRows.Close()
		for tRows.Next() {
			var cat, idx int64
			var txt string
			if err := tRows.Scan(&cat, &idx, &txt); err != nil {
				return nil, err
			}
			switch cat {
			case textCategoryCampaignTitle:
				titles[idx] = txt
			case textCategoryCampaignDesc:
				descs[idx] = txt
			}
		}
		if err := tRows.Err(); err != nil {
			return nil, err
		}
	}

	cols, _, err := tableMeta(db, "campaign_data")
	if err != nil {
		return nil, err
	}
	if len(cols) == 0 {
		return map[int64]campaignRecord{}, nil
	}

	colIndex := make(map[string]int, len(cols))
	for i, c := range cols {
		colIndex[c.Name] = i
	}
	sortedColIdx := make([]int, len(cols))
	for i := range cols {
		sortedColIdx[i] = i
	}
	sort.Slice(sortedColIdx, func(i, j int) bool {
		return cols[sortedColIdx[i]].Name < cols[sortedColIdx[j]].Name
	})

	rows, err := db.Query(`SELECT * FROM campaign_data WHERE effect_type_1 != 0 ORDER BY campaign_id ASC`)
	if err != nil {
		return nil, fmt.Errorf("query campaign_data: %w", err)
	}
	defer rows.Close()

	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}

	out := make(map[int64]campaignRecord)
	var rowBuf []byte
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}

		rowBuf = rowBuf[:0]
		for _, idx := range sortedColIdx {
			rowBuf = appendLengthPrefixedString(rowBuf, cols[idx].Name)
			rowBuf = appendEncodedValue(rowBuf, vals[idx])
		}

		campaignID, ok := asInt64(vals[colIndex["campaign_id"]])
		if !ok {
			return nil, fmt.Errorf("campaign_id has unexpected type %T", vals[colIndex["campaign_id"]])
		}
		effectType1, _ := asInt64(vals[colIndex["effect_type_1"]])
		startUnix, _ := asInt64(vals[colIndex["start_time"]])
		endUnix, _ := asInt64(vals[colIndex["end_time"]])

		ev := CampaignEvent{
			CampaignID:  campaignID,
			Title:       titles[campaignID],
			Description: descs[campaignID],
			EffectType1: effectType1,
			StartTime:   time.Unix(startUnix, 0).UTC(),
			EndTime:     time.Unix(endUnix, 0).UTC(),
		}
		out[campaignID] = campaignRecord{
			event:  ev,
			rowKey: string(rowBuf),
		}
	}
	return out, rows.Err()
}

func asInt64(v any) (int64, bool) {
	n, ok := v.(int64)
	return n, ok
}
