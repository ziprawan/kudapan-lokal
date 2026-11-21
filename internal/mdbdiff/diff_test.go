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
	if res.HasTableChanges() || !res.Campaign.IsEmpty() {
		t.Fatalf("expected empty diff for identical files, got %+v", res)
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
