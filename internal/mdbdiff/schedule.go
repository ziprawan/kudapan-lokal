package mdbdiff

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	textCategoryRaceTrack     = 35
	textCategoryScenarioTitle = 119
	textCategoryStoryEvent    = 189
	textCategoryChampionsCup  = 206
)

// EntryChange represents a modified record between old and new databases.
type EntryChange[T any] struct {
	Old T
	New T
}

// EntryDiff contains added, deleted, and changed records of type T.
type EntryDiff[T any] struct {
	Added   []T
	Deleted []T
	Changed []EntryChange[T]
}

// IsEmpty reports whether there are no added, deleted, or changed records.
func (d EntryDiff[T]) IsEmpty() bool {
	return len(d.Added) == 0 && len(d.Deleted) == 0 && len(d.Changed) == 0
}

// ScenarioEvent represents a single_mode_scenario record enriched with its
// title (text_data category 119).
type ScenarioEvent struct {
	ID         int64
	Title      string
	StartTime  time.Time
	EndTime    time.Time
	MaxSpeed   int64
	MaxStamina int64
	MaxPow     int64
	MaxGuts    int64
	MaxWiz     int64
}

// HasStatBonuses reports whether any scenario stat cap bonus is non-zero.
func (s ScenarioEvent) HasStatBonuses() bool {
	return s.MaxSpeed != 0 || s.MaxStamina != 0 || s.MaxPow != 0 || s.MaxGuts != 0 || s.MaxWiz != 0
}

// ScenarioDiff holds added, deleted, and changed single_mode_scenario rows.
type ScenarioDiff = EntryDiff[ScenarioEvent]

// ChampionsScheduleEvent represents a champions_schedule record enriched with
// its title (text_data category 206) and racecourse metadata when available.
type ChampionsScheduleEvent struct {
	ID        int64
	Title     string
	Course    string
	StartTime time.Time
	EndTime   time.Time
}

// ChampionsDiff holds added, deleted, and changed champions_schedule rows.
type ChampionsDiff = EntryDiff[ChampionsScheduleEvent]

// StoryEvent represents a story_event_data record enriched with its title
// (text_data category 189).
type StoryEvent struct {
	ID        int64
	Title     string
	StartTime time.Time
	EndTime   time.Time
}

// StoryEventDiff holds added, deleted, and changed story_event_data rows.
type StoryEventDiff = EntryDiff[StoryEvent]

// ChampionsRoundMilestone represents a round start milestone in
// champions_round_schedule (Round 1, Round 2, Round 3.1 Registration, or Round 3.3 Finals).
type ChampionsRoundMilestone struct {
	ID          int64
	ChampionsID int64
	Title       string
	Course      string
	Round       int64
	RoundDetail int64
	StartTime   time.Time
	EndTime     time.Time
}

// RoundLabel returns a human-readable label for the Champions Meeting round milestone.
func (m ChampionsRoundMilestone) RoundLabel() string {
	switch {
	case m.Round == 1 && m.RoundDetail == 0:
		return "Round 1"
	case m.Round == 2 && m.RoundDetail == 0:
		return "Round 2"
	case m.Round == 3 && m.RoundDetail == 1:
		return "Round 3 (Registration)"
	case m.Round == 3 && m.RoundDetail == 3:
		return "Round 3 (Finals)"
	default:
		return fmt.Sprintf("Round %d.%d", m.Round, m.RoundDetail)
	}
}

type entityRecord[T any] struct {
	item   T
	rowKey string
}

func loadTextLookup(db *sql.DB, categories ...int) (map[int]map[int64]string, error) {
	out := make(map[int]map[int64]string, len(categories))
	for _, cat := range categories {
		out[cat] = make(map[int64]string)
	}
	if len(categories) == 0 {
		return out, nil
	}

	hasText, err := tableExists(db, "text_data")
	if err != nil {
		return nil, err
	}
	if !hasText {
		return out, nil
	}

	placeholders := make([]string, len(categories))
	args := make([]any, len(categories))
	for i, cat := range categories {
		placeholders[i] = "?"
		args[i] = cat
	}
	query := fmt.Sprintf(`SELECT category, "index", text FROM text_data WHERE category IN (%s)`,
		strings.Join(placeholders, ", "))
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query text_data: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var cat int
		var idx int64
		var txt string
		if err := rows.Scan(&cat, &idx, &txt); err != nil {
			return nil, err
		}
		if m := out[cat]; m != nil {
			m[idx] = txt
		}
	}
	return out, rows.Err()
}

func loadTableEntities[T any](
	db *sql.DB,
	table string,
	query string,
	parse func(vals []any, colIndex map[string]int) (int64, T, error),
) (map[int64]entityRecord[T], error) {
	exists, err := tableExists(db, table)
	if err != nil {
		return nil, err
	}
	if !exists {
		return map[int64]entityRecord[T]{}, nil
	}

	cols, _, err := tableMeta(db, table)
	if err != nil {
		return nil, err
	}
	if len(cols) == 0 {
		return map[int64]entityRecord[T]{}, nil
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

	rows, err := db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("query %s: %w", table, err)
	}
	defer rows.Close()

	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}

	out := make(map[int64]entityRecord[T])
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

		id, item, err := parse(vals, colIndex)
		if err != nil {
			return nil, err
		}
		out[id] = entityRecord[T]{
			item:   item,
			rowKey: string(rowBuf),
		}
	}
	return out, rows.Err()
}

func diffEntityMaps[T any](
	oldMap, newMap map[int64]entityRecord[T],
	idOf func(T) int64,
) (added []T, deleted []T, changed []EntryChange[T]) {
	for id, newRec := range newMap {
		oldRec, ok := oldMap[id]
		if !ok {
			added = append(added, newRec.item)
			continue
		}
		if oldRec.rowKey != newRec.rowKey {
			changed = append(changed, EntryChange[T]{
				Old: oldRec.item,
				New: newRec.item,
			})
		}
	}
	for id, oldRec := range oldMap {
		if _, ok := newMap[id]; !ok {
			deleted = append(deleted, oldRec.item)
		}
	}

	sort.Slice(added, func(i, j int) bool { return idOf(added[i]) < idOf(added[j]) })
	sort.Slice(deleted, func(i, j int) bool { return idOf(deleted[i]) < idOf(deleted[j]) })
	sort.Slice(changed, func(i, j int) bool { return idOf(changed[i].New) < idOf(changed[j].New) })
	return added, deleted, changed
}

func diffTableEntry[T any](
	oldDB, newDB *sql.DB,
	label string,
	loadFn func(*sql.DB) (map[int64]entityRecord[T], error),
	idOf func(T) int64,
) (EntryDiff[T], error) {
	oldMap, err := loadFn(oldDB)
	if err != nil {
		return EntryDiff[T]{}, fmt.Errorf("load old %s: %w", label, err)
	}
	newMap, err := loadFn(newDB)
	if err != nil {
		return EntryDiff[T]{}, fmt.Errorf("load new %s: %w", label, err)
	}
	added, deleted, changed := diffEntityMaps(oldMap, newMap, idOf)
	return EntryDiff[T]{
		Added:   added,
		Deleted: deleted,
		Changed: changed,
	}, nil
}

func diffScenarioData(oldDB, newDB *sql.DB) (ScenarioDiff, error) {
	return diffTableEntry(oldDB, newDB, "single_mode_scenario", loadScenarios, func(ev ScenarioEvent) int64 {
		return ev.ID
	})
}

func diffChampionsScheduleData(oldDB, newDB *sql.DB) (ChampionsDiff, error) {
	return diffTableEntry(oldDB, newDB, "champions_schedule", loadChampionsSchedules, func(ev ChampionsScheduleEvent) int64 {
		return ev.ID
	})
}

func diffStoryEventData(oldDB, newDB *sql.DB) (StoryEventDiff, error) {
	return diffTableEntry(oldDB, newDB, "story_event_data", loadStoryEvents, func(ev StoryEvent) int64 {
		return ev.ID
	})
}

func loadScenarios(db *sql.DB) (map[int64]entityRecord[ScenarioEvent], error) {
	texts, err := loadTextLookup(db, textCategoryScenarioTitle)
	if err != nil {
		return nil, err
	}
	titles := texts[textCategoryScenarioTitle]

	return loadTableEntities(db, "single_mode_scenario",
		`SELECT * FROM single_mode_scenario ORDER BY id ASC`,
		func(vals []any, colIndex map[string]int) (int64, ScenarioEvent, error) {
			id, ok := colInt64(vals, colIndex, "id")
			if !ok {
				return 0, ScenarioEvent{}, fmt.Errorf("single_mode_scenario id missing or invalid")
			}
			startUnix, _ := colInt64(vals, colIndex, "start_date")
			endUnix, _ := colInt64(vals, colIndex, "end_date")
			maxSpeed, _ := colInt64(vals, colIndex, "max_speed")
			maxStamina, _ := colInt64(vals, colIndex, "max_stamina")
			maxPow, _ := colInt64(vals, colIndex, "max_pow")
			maxGuts, _ := colInt64(vals, colIndex, "max_guts")
			maxWiz, _ := colInt64(vals, colIndex, "max_wiz")

			return id, ScenarioEvent{
				ID:         id,
				Title:      normalizeSingleLineTitle(titles[id]),
				StartTime:  time.Unix(startUnix, 0).UTC(),
				EndTime:    time.Unix(endUnix, 0).UTC(),
				MaxSpeed:   maxSpeed,
				MaxStamina: maxStamina,
				MaxPow:     maxPow,
				MaxGuts:    maxGuts,
				MaxWiz:     maxWiz,
			}, nil
		})
}

func loadChampionsSchedules(db *sql.DB) (map[int64]entityRecord[ChampionsScheduleEvent], error) {
	texts, err := loadTextLookup(db, textCategoryChampionsCup)
	if err != nil {
		return nil, err
	}
	titles := texts[textCategoryChampionsCup]

	courses, err := loadChampionsCourseMap(db)
	if err != nil {
		return nil, err
	}

	return loadTableEntities(db, "champions_schedule",
		`SELECT * FROM champions_schedule ORDER BY id ASC`,
		func(vals []any, colIndex map[string]int) (int64, ChampionsScheduleEvent, error) {
			id, ok := colInt64(vals, colIndex, "id")
			if !ok {
				return 0, ChampionsScheduleEvent{}, fmt.Errorf("champions_schedule id missing or invalid")
			}
			startUnix, _ := colInt64(vals, colIndex, "start_date")
			endUnix, _ := colInt64(vals, colIndex, "end_date")

			return id, ChampionsScheduleEvent{
				ID:        id,
				Title:     normalizeSingleLineTitle(titles[id]),
				Course:    courses[id],
				StartTime: time.Unix(startUnix, 0).UTC(),
				EndTime:   time.Unix(endUnix, 0).UTC(),
			}, nil
		})
}

func loadChampionsCourseMap(db *sql.DB) (map[int64]string, error) {
	out := make(map[int64]string)
	for _, tbl := range []string{"champions_race_condition", "race_instance", "race", "race_course_set"} {
		ok, err := tableExists(db, tbl)
		if err != nil {
			return nil, err
		}
		if !ok {
			return out, nil
		}
	}

	texts, err := loadTextLookup(db, textCategoryRaceTrack)
	if err != nil {
		return nil, err
	}
	tracks := texts[textCategoryRaceTrack]

	rows, err := db.Query(`
		SELECT crc.champions_id, rcs.race_track_id, rcs.ground, rcs.distance
		FROM champions_race_condition crc
		JOIN race_instance ri ON ri.id = crc.race_instance_id
		JOIN race r ON r.id = ri.race_id
		JOIN race_course_set rcs ON rcs.id = r.course_set
		ORDER BY crc.champions_id ASC, crc.round_id ASC`)
	if err != nil {
		return nil, fmt.Errorf("query champions race courses: %w", err)
	}
	defer rows.Close()

	seen := make(map[int64]map[string]struct{})
	lists := make(map[int64][]string)
	for rows.Next() {
		var champID, trackID, ground, distance int64
		if err := rows.Scan(&champID, &trackID, &ground, &distance); err != nil {
			return nil, err
		}
		label := formatCourseLabel(tracks[trackID], trackID, ground, distance)
		if seen[champID] == nil {
			seen[champID] = make(map[string]struct{})
		}
		if _, exists := seen[champID][label]; !exists {
			seen[champID][label] = struct{}{}
			lists[champID] = append(lists[champID], label)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for id, labels := range lists {
		out[id] = strings.Join(labels, ", ")
	}
	return out, nil
}

func formatCourseLabel(trackName string, trackID, ground, distance int64) string {
	track := strings.TrimSpace(trackName)
	if track == "" {
		track = fmt.Sprintf("Track %d", trackID)
	}
	var surface string
	switch ground {
	case 1:
		surface = "Turf"
	case 2:
		surface = "Dirt"
	}
	if surface != "" {
		return fmt.Sprintf("%s %s %dm", track, surface, distance)
	}
	return fmt.Sprintf("%s %dm", track, distance)
}

func colInt64(vals []any, colIndex map[string]int, name string) (int64, bool) {
	pos, ok := colIndex[name]
	if !ok || pos < 0 || pos >= len(vals) {
		return 0, false
	}
	return asInt64(vals[pos])
}

func normalizeSingleLineTitle(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}

func loadStoryEvents(db *sql.DB) (map[int64]entityRecord[StoryEvent], error) {
	texts, err := loadTextLookup(db, textCategoryStoryEvent)
	if err != nil {
		return nil, err
	}
	titles := texts[textCategoryStoryEvent]

	return loadTableEntities(db, "story_event_data",
		`SELECT * FROM story_event_data ORDER BY story_event_id ASC`,
		func(vals []any, colIndex map[string]int) (int64, StoryEvent, error) {
			id, ok := colInt64(vals, colIndex, "story_event_id")
			if !ok {
				id, ok = colInt64(vals, colIndex, "id")
			}
			if !ok {
				return 0, StoryEvent{}, fmt.Errorf("story_event_data id missing or invalid")
			}
			startUnix, _ := colInt64(vals, colIndex, "start_date")
			endUnix, _ := colInt64(vals, colIndex, "end_date")

			return id, StoryEvent{
				ID:        id,
				Title:     normalizeSingleLineTitle(titles[id]),
				StartTime: time.Unix(startUnix, 0).UTC(),
				EndTime:   time.Unix(endUnix, 0).UTC(),
			}, nil
		})
}

const championsRoundMilestoneWhere = `((round IN (1, 2) AND round_detail = 0) OR (round = 3 AND round_detail IN (1, 3)))`

// LoadChampionsRoundMilestonesAfter opens dbPath read-only and returns all
// Champions Meeting round milestones (Round 1, Round 2, Round 3.1 Registration,
// and Round 3.3 Finals) with champions_round_schedule.id > afterID ordered by id ASC.
func LoadChampionsRoundMilestonesAfter(dbPath string, afterID int64) ([]ChampionsRoundMilestone, error) {
	db, err := openReadOnlySQLite(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	exists, err := tableExists(db, "champions_round_schedule")
	if err != nil || !exists {
		return nil, err
	}

	texts, err := loadTextLookup(db, textCategoryChampionsCup)
	if err != nil {
		return nil, err
	}
	titles := texts[textCategoryChampionsCup]

	courses, err := loadChampionsCourseMap(db)
	if err != nil {
		return nil, err
	}

	rows, err := db.Query(`
		SELECT id, champions_id, round, round_detail, start_date, end_date
		FROM champions_round_schedule
		WHERE id > ?
		  AND `+championsRoundMilestoneWhere+`
		ORDER BY id ASC`, afterID)
	if err != nil {
		return nil, fmt.Errorf("query champions_round_schedule: %w", err)
	}
	defer rows.Close()

	var out []ChampionsRoundMilestone
	for rows.Next() {
		var id, champID, round, detail, startUnix, endUnix int64
		if err := rows.Scan(&id, &champID, &round, &detail, &startUnix, &endUnix); err != nil {
			return nil, err
		}
		out = append(out, ChampionsRoundMilestone{
			ID:          id,
			ChampionsID: champID,
			Title:       normalizeSingleLineTitle(titles[champID]),
			Course:      courses[champID],
			Round:       round,
			RoundDetail: detail,
			StartTime:   time.Unix(startUnix, 0).UTC(),
			EndTime:     time.Unix(endUnix, 0).UTC(),
		})
	}
	return out, rows.Err()
}

// MaxPastChampionsRoundMilestoneID returns the highest milestone id in
// champions_round_schedule whose start_date is <= now (or 0 if none/missing).
func MaxPastChampionsRoundMilestoneID(dbPath string, now time.Time) (int64, error) {
	db, err := openReadOnlySQLite(dbPath)
	if err != nil {
		return 0, err
	}
	defer db.Close()

	exists, err := tableExists(db, "champions_round_schedule")
	if err != nil || !exists {
		return 0, err
	}

	var maxID int64
	err = db.QueryRow(`
		SELECT COALESCE(MAX(id), 0)
		FROM champions_round_schedule
		WHERE `+championsRoundMilestoneWhere+`
		  AND start_date <= ?`, now.UTC().Unix()).Scan(&maxID)
	if err != nil {
		return 0, fmt.Errorf("query max past champions_round_schedule id: %w", err)
	}
	return maxID, nil
}
