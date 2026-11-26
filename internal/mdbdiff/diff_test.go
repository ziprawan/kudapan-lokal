package mdbdiff

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestDiffFilesTablesAndCampaigns(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "old.mdb")
	newPath := filepath.Join(dir, "new.mdb")

	createTestDB(t, oldPath, []string{
		`CREATE TABLE single_pk (id INTEGER PRIMARY KEY, name TEXT NOT NULL)`,
		`CREATE TABLE text_data (id INTEGER NOT NULL, category INTEGER NOT NULL, "index" INTEGER NOT NULL, text TEXT NOT NULL, PRIMARY KEY(category, "index"))`,
		`CREATE TABLE keyless_tbl (grp INTEGER NOT NULL, val TEXT NOT NULL)`,
		`CREATE TABLE dropped_tbl (id INTEGER PRIMARY KEY)`,
		`CREATE TABLE campaign_data (campaign_id INTEGER PRIMARY KEY, effect_type_1 INTEGER NOT NULL, effect_value_1 INTEGER NOT NULL, start_time INTEGER NOT NULL, end_time INTEGER NOT NULL)`,
	}, []string{
		`INSERT INTO single_pk VALUES (1, 'unchanged'), (2, 'old_name'), (3, 'to_delete')`,
		`INSERT INTO text_data VALUES
			(187, 187, 10, 'Old Campaign'),
			(188, 188, 10, 'Old Desc'),
			(187, 187, 20, 'Deleted Campaign'),
			(188, 188, 20, 'Deleted Desc'),
			(187, 187, 30, 'Hidden Campaign'),
			(188, 188, 30, 'Hidden Desc')`,
		`INSERT INTO keyless_tbl VALUES (1, 'a'), (1, 'b'), (2, 'c')`,
		`INSERT INTO dropped_tbl VALUES (100), (200)`,
		`INSERT INTO campaign_data VALUES
			(10, 2, 3, 1700000000, 1700500000),
			(20, 5, 1, 1699000000, 1699500000),
			(30, 0, 0, 1700000000, 1700500000)`,
	})

	createTestDB(t, newPath, []string{
		`CREATE TABLE single_pk (id INTEGER PRIMARY KEY, name TEXT NOT NULL)`,
		`CREATE TABLE text_data (id INTEGER NOT NULL, category INTEGER NOT NULL, "index" INTEGER NOT NULL, text TEXT NOT NULL, PRIMARY KEY(category, "index"))`,
		`CREATE TABLE keyless_tbl (grp INTEGER NOT NULL, val TEXT NOT NULL)`,
		`CREATE TABLE added_tbl (id INTEGER PRIMARY KEY)`,
		`CREATE TABLE campaign_data (campaign_id INTEGER PRIMARY KEY, effect_type_1 INTEGER NOT NULL, effect_value_1 INTEGER NOT NULL, start_time INTEGER NOT NULL, end_time INTEGER NOT NULL)`,
	}, []string{
		`INSERT INTO single_pk VALUES (1, 'unchanged'), (2, 'new_name'), (4, 'added')`,
		`INSERT INTO text_data VALUES
			(187, 187, 10, 'Old Campaign'),
			(188, 188, 10, 'Old Desc'),
			(187, 187, 30, 'Hidden Campaign'),
			(188, 188, 30, 'Hidden Desc'),
			(187, 187, 40, 'New Campaign'),
			(188, 188, 40, 'New Desc')`,
		// Reordered identical rows plus one added and one deleted.
		`INSERT INTO keyless_tbl VALUES (1, 'b'), (1, 'a'), (3, 'd')`,
		`INSERT INTO added_tbl VALUES (1)`,
		// Campaign 10 changed end_time; 20 deleted; 30 changed (effect_type_1=0, should count in TableDiff only); 40 added; 50 added with effect_type_1=0 (should count in TableDiff only).
		`INSERT INTO campaign_data VALUES
			(10, 2, 3, 1700000000, 1700600000),
			(30, 0, 99, 1700000000, 1700500000),
			(40, 1, 200, 1701000000, 1701500000),
			(50, 0, 0, 1701000000, 1701500000)`,
	})

	res, err := DiffFiles(oldPath, newPath)
	if err != nil {
		t.Fatalf("DiffFiles() error = %v", err)
	}

	gotTables := make(map[string]TableDiff)
	for _, td := range res.Tables {
		gotTables[td.Table] = td
	}

	wantTables := map[string]TableDiff{
		"added_tbl":     {Table: "added_tbl", Added: 1},
		"campaign_data": {Table: "campaign_data", Added: 2, Deleted: 1, Changed: 2},
		"dropped_tbl":   {Table: "dropped_tbl", Deleted: 2},
		"keyless_tbl":   {Table: "keyless_tbl", Added: 1, Deleted: 1},
		"single_pk":     {Table: "single_pk", Added: 1, Deleted: 1, Changed: 1},
		"text_data":     {Table: "text_data", Added: 2, Deleted: 2},
	}
	if len(gotTables) != len(wantTables) {
		t.Fatalf("diff tables count = %d (%+v), want %d (%+v)", len(gotTables), gotTables, len(wantTables), wantTables)
	}
	for name, want := range wantTables {
		if gotTables[name] != want {
			t.Errorf("table %q = %+v, want %+v", name, gotTables[name], want)
		}
	}

	// Verify campaign_data detail diff filters out effect_type_1 = 0 (IDs 30 and 50).
	if len(res.Campaign.Added) != 1 || res.Campaign.Added[0].CampaignID != 40 {
		t.Fatalf("Campaign.Added = %+v, want single ID 40", res.Campaign.Added)
	}
	if res.Campaign.Added[0].Title != "New Campaign" || res.Campaign.Added[0].Description != "New Desc" {
		t.Errorf("Added campaign text = (%q, %q)", res.Campaign.Added[0].Title, res.Campaign.Added[0].Description)
	}
	if !res.Campaign.Added[0].StartTime.Equal(time.Unix(1701000000, 0).UTC()) {
		t.Errorf("Added StartTime = %v", res.Campaign.Added[0].StartTime)
	}

	if len(res.Campaign.Deleted) != 1 || res.Campaign.Deleted[0].CampaignID != 20 {
		t.Fatalf("Campaign.Deleted = %+v, want single ID 20", res.Campaign.Deleted)
	}
	if res.Campaign.Deleted[0].Title != "Deleted Campaign" || res.Campaign.Deleted[0].Description != "Deleted Desc" {
		t.Errorf("Deleted campaign text = (%q, %q)", res.Campaign.Deleted[0].Title, res.Campaign.Deleted[0].Description)
	}

	if len(res.Campaign.Changed) != 1 || res.Campaign.Changed[0].New.CampaignID != 10 {
		t.Fatalf("Campaign.Changed = %+v, want single ID 10", res.Campaign.Changed)
	}
	if res.Campaign.Changed[0].Old.EndTime.Unix() != 1700500000 || res.Campaign.Changed[0].New.EndTime.Unix() != 1700600000 {
		t.Errorf("Changed EndTime = %v -> %v", res.Campaign.Changed[0].Old.EndTime, res.Campaign.Changed[0].New.EndTime)
	}
}

func TestDiffFilesIdenticalShortCircuits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "same.mdb")
	createTestDB(t, path, []string{`CREATE TABLE t (id INTEGER PRIMARY KEY)`}, []string{`INSERT INTO t VALUES (1)`})

	res, err := DiffFiles(path, path)
	if err != nil {
		t.Fatalf("DiffFiles() error = %v", err)
	}
	if res.HasTableChanges() || !res.Campaign.IsEmpty() || !res.Announce.IsEmpty() || !res.Scenario.IsEmpty() || !res.Champions.IsEmpty() || !res.StoryEvent.IsEmpty() {
		t.Fatalf("expected empty diff for identical files, got %+v", res)
	}
}

func TestDiffScenarioAndChampionsSchedule(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "old.mdb")
	newPath := filepath.Join(dir, "new.mdb")

	schema := []string{
		`CREATE TABLE text_data (id INTEGER NOT NULL, category INTEGER NOT NULL, "index" INTEGER NOT NULL, text TEXT NOT NULL, PRIMARY KEY(category, "index"))`,
		`CREATE TABLE single_mode_scenario (id INTEGER PRIMARY KEY, start_date INTEGER NOT NULL, end_date INTEGER NOT NULL, max_speed INTEGER NOT NULL, max_stamina INTEGER NOT NULL, max_pow INTEGER NOT NULL, max_guts INTEGER NOT NULL, max_wiz INTEGER NOT NULL)`,
		`CREATE TABLE champions_schedule (id INTEGER PRIMARY KEY, resource_id INTEGER NOT NULL, start_date INTEGER NOT NULL, end_date INTEGER NOT NULL)`,
		`CREATE TABLE champions_race_condition (champions_id INTEGER NOT NULL, round_id INTEGER NOT NULL, race_instance_id INTEGER NOT NULL, race_condition_id INTEGER NOT NULL, PRIMARY KEY(champions_id, round_id))`,
		`CREATE TABLE race_instance (id INTEGER PRIMARY KEY, race_id INTEGER NOT NULL)`,
		`CREATE TABLE race (id INTEGER PRIMARY KEY, course_set INTEGER NOT NULL)`,
		`CREATE TABLE race_course_set (id INTEGER PRIMARY KEY, race_track_id INTEGER NOT NULL, distance INTEGER NOT NULL, ground INTEGER NOT NULL)`,
	}

	createTestDB(t, oldPath, schema, []string{
		`INSERT INTO text_data VALUES
			(119, 119, 1, 'URA Finale'),
			(119, 119, 2, 'Unity Cup'),
			(206, 206, 21, 'Capricorn Cup'),
			(206, 206, 22, 'Aquarius Cup'),
			(35, 35, 10006, 'Tokyo'),
			(35, 35, 10007, 'Chukyo')`,
		`INSERT INTO single_mode_scenario VALUES
			(1, 1700000000, 1800000000, 200, 200, 200, 200, 200),
			(2, 1710000000, 1800000000, 100, 100, 100, 100, 600)`,
		`INSERT INTO champions_schedule VALUES
			(21, 1, 1793138400, 1794002399),
			(22, 2, 1794952800, 1795816799)`,
		`INSERT INTO champions_race_condition VALUES (21, 0, 1, 1), (22, 0, 2, 1)`,
		`INSERT INTO race_instance VALUES (1, 101), (2, 102)`,
		`INSERT INTO race VALUES (101, 201), (102, 202)`,
		`INSERT INTO race_course_set VALUES (201, 10007, 1200, 1), (202, 10006, 1600, 2)`,
	})

	createTestDB(t, newPath, schema, []string{
		`INSERT INTO text_data VALUES
			(119, 119, 1, 'URA Finale'),
			(119, 119, 5, 'Grandmasters' || char(10) || 'Legacies Immortal'),
			(206, 206, 21, 'Capricorn Cup'),
			(206, 206, 23, 'Pisces Cup'),
			(35, 35, 10006, 'Tokyo'),
			(35, 35, 10007, 'Chukyo'),
			(35, 35, 10009, 'Hanshin')`,
		`INSERT INTO single_mode_scenario VALUES
			(1, 1700000000, 1850000000, 200, 200, 200, 200, 200),
			(5, 1795000000, 2524608000, 300, 300, 300, 300, 400)`,
		`INSERT INTO champions_schedule VALUES
			(21, 1, 1793138400, 1794090000),
			(23, 3, 1796500000, 1797300000)`,
		`INSERT INTO champions_race_condition VALUES (21, 0, 1, 1), (23, 0, 3, 1)`,
		`INSERT INTO race_instance VALUES (1, 101), (3, 103)`,
		`INSERT INTO race VALUES (101, 201), (103, 203)`,
		`INSERT INTO race_course_set VALUES (201, 10007, 1200, 1), (203, 10009, 2000, 1)`,
	})

	res, err := DiffFiles(oldPath, newPath)
	if err != nil {
		t.Fatalf("DiffFiles() error = %v", err)
	}

	if len(res.Scenario.Added) != 1 || res.Scenario.Added[0].ID != 5 || res.Scenario.Added[0].Title != "Grandmasters Legacies Immortal" {
		t.Fatalf("unexpected Scenario.Added: %+v", res.Scenario.Added)
	}
	if len(res.Scenario.Deleted) != 1 || res.Scenario.Deleted[0].ID != 2 || res.Scenario.Deleted[0].Title != "Unity Cup" {
		t.Fatalf("unexpected Scenario.Deleted: %+v", res.Scenario.Deleted)
	}
	if len(res.Scenario.Changed) != 1 || res.Scenario.Changed[0].New.EndTime.Unix() != 1850000000 {
		t.Fatalf("unexpected Scenario.Changed: %+v", res.Scenario.Changed)
	}

	if len(res.Champions.Added) != 1 || res.Champions.Added[0].ID != 23 || res.Champions.Added[0].Title != "Pisces Cup" || res.Champions.Added[0].Course != "Hanshin Turf 2000m" {
		t.Fatalf("unexpected Champions.Added: %+v", res.Champions.Added)
	}
	if len(res.Champions.Deleted) != 1 || res.Champions.Deleted[0].ID != 22 || res.Champions.Deleted[0].Title != "Aquarius Cup" || res.Champions.Deleted[0].Course != "Tokyo Dirt 1600m" {
		t.Fatalf("unexpected Champions.Deleted: %+v", res.Champions.Deleted)
	}
	if len(res.Champions.Changed) != 1 || res.Champions.Changed[0].New.ID != 21 || res.Champions.Changed[0].New.Course != "Chukyo Turf 1200m" {
		t.Fatalf("unexpected Champions.Changed: %+v", res.Champions.Changed)
	}
}

func TestDiffAnnounceData(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "old.mdb")
	newPath := filepath.Join(dir, "new.mdb")

	schema := []string{
		`CREATE TABLE text_data (id INTEGER NOT NULL, category INTEGER NOT NULL, "index" INTEGER NOT NULL, text TEXT NOT NULL, PRIMARY KEY(category, "index"))`,
		`CREATE TABLE announce_data (id INTEGER PRIMARY KEY, announce_type INTEGER NOT NULL, announce_id INTEGER NOT NULL, priority INTEGER NOT NULL, start_date INTEGER NOT NULL, end_date INTEGER NOT NULL)`,
		`CREATE TABLE announce_character (id INTEGER PRIMARY KEY, movie_id INTEGER NOT NULL, can_end_second INTEGER NOT NULL)`,
		`CREATE TABLE announce_support_card (id INTEGER PRIMARY KEY, type INTEGER NOT NULL, cuts_pattern INTEGER NOT NULL, support_card_id_a INTEGER NOT NULL, support_card_id_b INTEGER NOT NULL, support_card_id_c INTEGER NOT NULL)`,
		`CREATE TABLE gacha_available (gacha_id INTEGER NOT NULL, card_id INTEGER NOT NULL, card_type INTEGER NOT NULL, rarity INTEGER NOT NULL, odds INTEGER NOT NULL, is_pickup INTEGER NOT NULL, recommend_order INTEGER NOT NULL, is_prize_selectable INTEGER NOT NULL, PRIMARY KEY(gacha_id, card_id))`,
	}

	createTestDB(t, oldPath, schema, []string{
		`INSERT INTO text_data VALUES
			(4, 4, 101501, '[O Sole Suo!] T.M. Opera O'),
			(75, 75, 30024, '[Get Lots of Hugs for Me] Oguri Cap'),
			(75, 75, 20023, '[Lamplit Training of a Witch-to-Be] Sweep Tosho')`,
		`INSERT INTO announce_character VALUES (101, 30004, 6000)`,
		`INSERT INTO announce_support_card VALUES (501, 1, 2, 30024, 20023, 0)`,
		`INSERT INTO gacha_available VALUES (30004, 101501, 1, 3, 75000, 1, 1, 0)`,
		`INSERT INTO announce_data VALUES
			(30004, 1, 101, 1, 1750897800, 1751493599),
			(30005, 2, 501, 1, 1750897800, 1751493599)`,
	})

	createTestDB(t, newPath, schema, []string{
		`INSERT INTO text_data VALUES
			(4, 4, 101501, '[O Sole Suo!] T.M. Opera O'),
			(4, 4, 100302, '[Beyond the Horizon] Tokai Teio'),
			(4, 4, 101302, '[End of the Skies] Mejiro McQueen'),
			(75, 75, 30028, '[Fire at My Heels] Kitasan Black'),
			(75, 75, 30029, '[The Will to Overtake] Satono Diamond')`,
		`INSERT INTO announce_character VALUES (101, 30004, 6000), (104, 30010, 11800)`,
		`INSERT INTO announce_support_card VALUES (504, 3, 2, 30028, 30029, 0)`,
		`INSERT INTO gacha_available VALUES
			(30004, 101501, 1, 3, 75000, 1, 1, 0),
			(30010, 101302, 1, 3, 75000, 1, 2, 0),
			(30010, 100302, 1, 3, 75000, 1, 1, 0),
			(30010, 100101, 1, 3, 10000, 0, 0, 0)`,
		`INSERT INTO announce_data VALUES
			(30004, 1, 101, 1, 1750897800, 1751580000),
			(30010, 1, 104, 1, 1752703200, 1754085599),
			(30011, 2, 504, 1, 1752703200, 1754085599)`,
	})

	res, err := DiffFiles(oldPath, newPath)
	if err != nil {
		t.Fatalf("DiffFiles() error = %v", err)
	}

	if len(res.Announce.Added) != 2 {
		t.Fatalf("Announce.Added count = %d (%+v), want 2", len(res.Announce.Added), res.Announce.Added)
	}
	if res.Announce.Added[0].ID != 30010 || res.Announce.Added[0].AnnounceType != AnnounceTypeCharacter ||
		res.Announce.Added[0].Title != "[Beyond the Horizon] Tokai Teio, [End of the Skies] Mejiro McQueen" {
		t.Errorf("unexpected added character announce: %+v", res.Announce.Added[0])
	}
	if res.Announce.Added[1].ID != 30011 || res.Announce.Added[1].AnnounceType != AnnounceTypeSupportCard ||
		res.Announce.Added[1].Title != "[Fire at My Heels] Kitasan Black, [The Will to Overtake] Satono Diamond" {
		t.Errorf("unexpected added support announce: %+v", res.Announce.Added[1])
	}

	if len(res.Announce.Deleted) != 1 || res.Announce.Deleted[0].ID != 30005 ||
		res.Announce.Deleted[0].Title != "[Get Lots of Hugs for Me] Oguri Cap, [Lamplit Training of a Witch-to-Be] Sweep Tosho" {
		t.Errorf("unexpected deleted announce: %+v", res.Announce.Deleted)
	}

	if len(res.Announce.Changed) != 1 || res.Announce.Changed[0].New.ID != 30004 ||
		res.Announce.Changed[0].New.Title != "[O Sole Suo!] T.M. Opera O" ||
		res.Announce.Changed[0].New.EndTime.Unix() != 1751580000 {
		t.Errorf("unexpected changed announce: %+v", res.Announce.Changed)
	}
}

func TestDiffStoryEventData(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "old.mdb")
	newPath := filepath.Join(dir, "new.mdb")

	schema := []string{
		`CREATE TABLE text_data (id INTEGER NOT NULL, category INTEGER NOT NULL, "index" INTEGER NOT NULL, text TEXT NOT NULL, PRIMARY KEY(category, "index"))`,
		`CREATE TABLE story_event_data (story_event_id INTEGER PRIMARY KEY, announce_id INTEGER NOT NULL, consume_tp_ratio INTEGER NOT NULL, bonus_reward_ratio INTEGER NOT NULL, notice_date INTEGER NOT NULL, start_date INTEGER NOT NULL, middle_date_01 INTEGER NOT NULL, middle_date_02 INTEGER NOT NULL, ending_date INTEGER NOT NULL, end_date INTEGER NOT NULL)`,
	}

	createTestDB(t, oldPath, schema, []string{
		`INSERT INTO text_data VALUES
			(189, 189, 1020, 'Illuminate the Heart'),
			(189, 189, 1021, 'Old Deleted Story')`,
		`INSERT INTO story_event_data VALUES
			(1020, 10, 100, 100, 1790287200, 1790632800, 1791000000, 1791400000, 1791842399, 1792101599),
			(1021, 11, 100, 100, 1792188000, 1792533600, 1792800000, 1793200000, 1793570399, 1793829599)`,
	})

	createTestDB(t, newPath, schema, []string{
		`INSERT INTO text_data VALUES
			(189, 189, 1020, 'Illuminate the Heart'),
			(189, 189, 1023, 'Leap into a New World!')`,
		`INSERT INTO story_event_data VALUES
			(1020, 10, 100, 100, 1790287200, 1790632800, 1791000000, 1791400000, 1791842399, 1792200000),
			(1023, 13, 100, 100, 1795816800, 1796162400, 1796500000, 1796900000, 1797371999, 1797631199)`,
	})

	res, err := DiffFiles(oldPath, newPath)
	if err != nil {
		t.Fatalf("DiffFiles() error = %v", err)
	}
	if len(res.StoryEvent.Added) != 1 || res.StoryEvent.Added[0].ID != 1023 || res.StoryEvent.Added[0].Title != "Leap into a New World!" {
		t.Fatalf("unexpected StoryEvent.Added: %+v", res.StoryEvent.Added)
	}
	if len(res.StoryEvent.Deleted) != 1 || res.StoryEvent.Deleted[0].ID != 1021 || res.StoryEvent.Deleted[0].Title != "Old Deleted Story" {
		t.Fatalf("unexpected StoryEvent.Deleted: %+v", res.StoryEvent.Deleted)
	}
	if len(res.StoryEvent.Changed) != 1 || res.StoryEvent.Changed[0].New.ID != 1020 || res.StoryEvent.Changed[0].New.EndTime.Unix() != 1792200000 {
		t.Fatalf("unexpected StoryEvent.Changed: %+v", res.StoryEvent.Changed)
	}
}

func createTestDB(t *testing.T, path string, ddls, inserts []string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open(%q) error = %v", path, err)
	}
	defer db.Close()

	for _, ddl := range ddls {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatalf("exec DDL %q: %v", ddl, err)
		}
	}
	for _, ins := range inserts {
		if _, err := db.Exec(ins); err != nil {
			t.Fatalf("exec INSERT %q: %v", ins, err)
		}
	}
}
