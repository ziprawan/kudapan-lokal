package mdbdiff

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const (
	textCategoryCardTitle        = 4
	textCategorySupportCardTitle = 75
)

// Known announce_data.announce_type values.
const (
	AnnounceTypeCharacter   = 1
	AnnounceTypeSupportCard = 2
)

// AnnounceEvent represents an announce_data record enriched with featured
// character or support card names resolved from announce_character/gacha_available
// or announce_support_card and text_data.
type AnnounceEvent struct {
	ID           int64
	AnnounceType int64
	AnnounceID   int64
	Title        string
	StartTime    time.Time
	EndTime      time.Time
}

// TypeLabel returns a human-readable label for AnnounceType.
func (e AnnounceEvent) TypeLabel() string {
	switch e.AnnounceType {
	case AnnounceTypeCharacter:
		return "Character"
	case AnnounceTypeSupportCard:
		return "Support Card"
	default:
		return fmt.Sprintf("Type %d", e.AnnounceType)
	}
}

// AnnounceDiff holds added, deleted, and changed announce_data rows.
type AnnounceDiff = EntryDiff[AnnounceEvent]

func diffAnnounceData(oldDB, newDB *sql.DB) (AnnounceDiff, error) {
	return diffTableEntry(oldDB, newDB, "announce_data", loadAnnounces, func(ev AnnounceEvent) int64 {
		return ev.ID
	})
}

func loadAnnounces(db *sql.DB) (map[int64]entityRecord[AnnounceEvent], error) {
	texts, err := loadTextLookup(db, textCategoryCardTitle, textCategorySupportCardTitle)
	if err != nil {
		return nil, err
	}
	cardTitles := texts[textCategoryCardTitle]
	supportTitles := texts[textCategorySupportCardTitle]

	movieIDs, err := loadAnnounceCharacterMovies(db)
	if err != nil {
		return nil, err
	}
	supportCards, err := loadAnnounceSupportCards(db)
	if err != nil {
		return nil, err
	}
	pickups, err := loadGachaPickups(db)
	if err != nil {
		return nil, err
	}

	return loadTableEntities(db, "announce_data",
		`SELECT * FROM announce_data ORDER BY id ASC`,
		func(vals []any, colIndex map[string]int) (int64, AnnounceEvent, error) {
			id, ok := colInt64(vals, colIndex, "id")
			if !ok {
				return 0, AnnounceEvent{}, fmt.Errorf("announce_data id missing or invalid")
			}
			announceType, _ := colInt64(vals, colIndex, "announce_type")
			announceID, _ := colInt64(vals, colIndex, "announce_id")
			startUnix, _ := colInt64(vals, colIndex, "start_date")
			endUnix, _ := colInt64(vals, colIndex, "end_date")

			var title string
			switch announceType {
			case AnnounceTypeCharacter:
				gachaID := movieIDs[announceID]
				if gachaID == 0 {
					gachaID = id
				}
				title = resolveAnnounceTitles(pickups[gachaID], cardTitles, "Card")
			case AnnounceTypeSupportCard:
				cardIDs := supportCards[announceID]
				if len(cardIDs) == 0 {
					cardIDs = pickups[id]
				}
				title = resolveAnnounceTitles(cardIDs, supportTitles, "Support Card")
			}

			return id, AnnounceEvent{
				ID:           id,
				AnnounceType: announceType,
				AnnounceID:   announceID,
				Title:        title,
				StartTime:    time.Unix(startUnix, 0).UTC(),
				EndTime:      time.Unix(endUnix, 0).UTC(),
			}, nil
		})
}

func loadAnnounceCharacterMovies(db *sql.DB) (map[int64]int64, error) {
	out := make(map[int64]int64)
	ok, err := tableExists(db, "announce_character")
	if err != nil || !ok {
		return out, err
	}

	rows, err := db.Query(`SELECT id, movie_id FROM announce_character`)
	if err != nil {
		return nil, fmt.Errorf("query announce_character: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id, movieID int64
		if err := rows.Scan(&id, &movieID); err != nil {
			return nil, err
		}
		out[id] = movieID
	}
	return out, rows.Err()
}

func loadAnnounceSupportCards(db *sql.DB) (map[int64][]int64, error) {
	out := make(map[int64][]int64)
	ok, err := tableExists(db, "announce_support_card")
	if err != nil || !ok {
		return out, err
	}

	rows, err := db.Query(`SELECT id, support_card_id_a, support_card_id_b, support_card_id_c FROM announce_support_card`)
	if err != nil {
		return nil, fmt.Errorf("query announce_support_card: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id, a, b, c int64
		if err := rows.Scan(&id, &a, &b, &c); err != nil {
			return nil, err
		}
		var ids []int64
		for _, cardID := range []int64{a, b, c} {
			if cardID > 0 {
				ids = append(ids, cardID)
			}
		}
		out[id] = ids
	}
	return out, rows.Err()
}

func loadGachaPickups(db *sql.DB) (map[int64][]int64, error) {
	out := make(map[int64][]int64)
	ok, err := tableExists(db, "gacha_available")
	if err != nil || !ok {
		return out, err
	}

	rows, err := db.Query(`
		SELECT gacha_id, card_id
		FROM gacha_available
		WHERE is_pickup = 1
		ORDER BY gacha_id ASC, recommend_order ASC, card_id ASC`)
	if err != nil {
		return nil, fmt.Errorf("query gacha_available pickups: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var gachaID, cardID int64
		if err := rows.Scan(&gachaID, &cardID); err != nil {
			return nil, err
		}
		out[gachaID] = append(out[gachaID], cardID)
	}
	return out, rows.Err()
}

func resolveAnnounceTitles(ids []int64, titles map[int64]string, fallbackPrefix string) string {
	if len(ids) == 0 {
		return ""
	}
	names := make([]string, 0, len(ids))
	for _, id := range ids {
		name := normalizeSingleLineTitle(titles[id])
		if name == "" {
			name = fmt.Sprintf("%s #%d", fallbackPrefix, id)
		}
		names = append(names, name)
	}
	return strings.Join(names, ", ")
}
