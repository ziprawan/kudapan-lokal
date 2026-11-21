// Package mdbdiff compares Uma Musume master.mdb SQLite databases and extracts
// detailed campaign_data diffs joined with text_data titles and descriptions.
package mdbdiff

import (
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// TableDiff summarizes row-level differences for a single SQLite table.
type TableDiff struct {
	Table   string
	Added   int
	Deleted int
	Changed int
}

// HasChanges reports whether the table has any added, deleted, or changed rows.
func (d TableDiff) HasChanges() bool {
	return d.Added > 0 || d.Deleted > 0 || d.Changed > 0
}

// DiffResult contains the full table diff summary and detailed campaign_data diff.
type DiffResult struct {
	Tables   []TableDiff
	Campaign CampaignDiff
}

// HasTableChanges reports whether any table in master.mdb differed.
func (r *DiffResult) HasTableChanges() bool {
	return len(r.Tables) > 0
}

type colMeta struct {
	Name string
	PK   int
}

// DiffFiles opens oldPath and newPath as read-only SQLite databases and
// computes per-table row diffs plus detailed campaign_data diffs.
func DiffFiles(oldPath, newPath string) (*DiffResult, error) {
	same, err := filesIdentical(oldPath, newPath)
	if err != nil {
		return nil, err
	}
	if same {
		return &DiffResult{}, nil
	}

	oldDB, err := openReadOnlySQLite(oldPath)
	if err != nil {
		return nil, fmt.Errorf("open old master.mdb %q: %w", oldPath, err)
	}
	defer oldDB.Close()

	newDB, err := openReadOnlySQLite(newPath)
	if err != nil {
		return nil, fmt.Errorf("open new master.mdb %q: %w", newPath, err)
	}
	defer newDB.Close()

	oldTables, err := listTables(oldDB)
	if err != nil {
		return nil, fmt.Errorf("list tables in old db: %w", err)
	}
	newTables, err := listTables(newDB)
	if err != nil {
		return nil, fmt.Errorf("list tables in new db: %w", err)
	}

	oldSet := make(map[string]struct{}, len(oldTables))
	for _, t := range oldTables {
		oldSet[t] = struct{}{}
	}
	newSet := make(map[string]struct{}, len(newTables))
	for _, t := range newTables {
		newSet[t] = struct{}{}
	}

	allTablesMap := make(map[string]struct{}, len(oldTables)+len(newTables))
	for _, t := range oldTables {
		allTablesMap[t] = struct{}{}
	}
	for _, t := range newTables {
		allTablesMap[t] = struct{}{}
	}
	allTables := make([]string, 0, len(allTablesMap))
	for t := range allTablesMap {
		allTables = append(allTables, t)
	}
	sort.Strings(allTables)

	var tableDiffs []TableDiff
	for _, table := range allTables {
		_, inOld := oldSet[table]
		_, inNew := newSet[table]

		var td TableDiff
		switch {
		case !inOld && inNew:
			cnt, err := countRows(newDB, table)
			if err != nil {
				return nil, fmt.Errorf("count new table %q: %w", table, err)
			}
			td = TableDiff{Table: table, Added: cnt}
		case inOld && !inNew:
			cnt, err := countRows(oldDB, table)
			if err != nil {
				return nil, fmt.Errorf("count old table %q: %w", table, err)
			}
			td = TableDiff{Table: table, Deleted: cnt}
		default:
			td, err = diffTable(oldDB, newDB, table)
			if err != nil {
				return nil, fmt.Errorf("diff table %q: %w", table, err)
			}
		}

		if td.HasChanges() {
			tableDiffs = append(tableDiffs, td)
		}
	}

	campaignDiff, err := diffCampaignData(oldDB, newDB)
	if err != nil {
		return nil, fmt.Errorf("diff campaign_data: %w", err)
	}

	return &DiffResult{
		Tables:   tableDiffs,
		Campaign: campaignDiff,
	}, nil
}

func openReadOnlySQLite(path string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(abs); err != nil {
		return nil, err
	}
	dsn := (&url.URL{
		Scheme:   "file",
		Path:     filepath.ToSlash(abs),
		RawQuery: "mode=ro",
	}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func filesIdentical(a, b string) (bool, error) {
	ha, err := fileSHA256(a)
	if err != nil {
		return false, err
	}
	hb, err := fileSHA256(b)
	if err != nil {
		return false, err
	}
	return ha == hb, nil
}

func fileSHA256(path string) ([32]byte, error) {
	var sum [32]byte
	f, err := os.Open(path)
	if err != nil {
		return sum, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return sum, err
	}
	copy(sum[:], h.Sum(nil))
	return sum, nil
}

func listTables(db *sql.DB) ([]string, error) {
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		tables = append(tables, name)
	}
	return tables, rows.Err()
}

func tableExists(db *sql.DB, name string) (bool, error) {
	var cnt int
	err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&cnt)
	return cnt > 0, err
}

func tableMeta(db *sql.DB, table string) (cols []colMeta, pkNames []string, err error) {
	rows, err := db.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, quoteIdent(table)))
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	var pkCols []colMeta
	for rows.Next() {
		var (
			cid      int
			name     string
			colType  string
			notNull  int
			dflt     sql.NullString
			pkOrd    int
		)
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pkOrd); err != nil {
			return nil, nil, err
		}
		cm := colMeta{Name: name, PK: pkOrd}
		cols = append(cols, cm)
		if pkOrd > 0 {
			pkCols = append(pkCols, cm)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	sort.Slice(pkCols, func(i, j int) bool {
		return pkCols[i].PK < pkCols[j].PK
	})
	pkNames = make([]string, len(pkCols))
	for i, c := range pkCols {
		pkNames[i] = c.Name
	}
	return cols, pkNames, nil
}

func countRows(db *sql.DB, table string) (int, error) {
	var cnt int
	err := db.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM %s`, quoteIdent(table))).Scan(&cnt)
	return cnt, err
}

func diffTable(oldDB, newDB *sql.DB, table string) (TableDiff, error) {
	oldCols, oldPKs, err := tableMeta(oldDB, table)
	if err != nil {
		return TableDiff{}, err
	}
	newCols, newPKs, err := tableMeta(newDB, table)
	if err != nil {
		return TableDiff{}, err
	}

	if len(oldPKs) > 0 && stringSlicesEqual(oldPKs, newPKs) {
		return diffKeyedTable(oldDB, newDB, table, oldCols, newCols, oldPKs)
	}
	return diffKeylessTable(oldDB, newDB, table, oldCols, newCols)
}

func diffKeyedTable(oldDB, newDB *sql.DB, table string, oldCols, newCols []colMeta, pkNames []string) (TableDiff, error) {
	oldMap := make(map[string]string)
	if err := streamRows(oldDB, table, oldCols, pkNames, func(pkKey, rowKey string) {
		oldMap[pkKey] = rowKey
	}); err != nil {
		return TableDiff{}, err
	}

	td := TableDiff{Table: table}
	if err := streamRows(newDB, table, newCols, pkNames, func(pkKey, rowKey string) {
		oldRowKey, ok := oldMap[pkKey]
		if !ok {
			td.Added++
			return
		}
		if oldRowKey != rowKey {
			td.Changed++
		}
		delete(oldMap, pkKey)
	}); err != nil {
		return TableDiff{}, err
	}

	td.Deleted = len(oldMap)
	return td, nil
}

func diffKeylessTable(oldDB, newDB *sql.DB, table string, oldCols, newCols []colMeta) (TableDiff, error) {
	oldCounts := make(map[string]int)
	if err := streamRows(oldDB, table, oldCols, nil, func(_, rowKey string) {
		oldCounts[rowKey]++
	}); err != nil {
		return TableDiff{}, err
	}

	td := TableDiff{Table: table}
	if err := streamRows(newDB, table, newCols, nil, func(_, rowKey string) {
		if oldCounts[rowKey] > 0 {
			oldCounts[rowKey]--
			return
		}
		td.Added++
	}); err != nil {
		return TableDiff{}, err
	}

	for _, rem := range oldCounts {
		td.Deleted += rem
	}
	return td, nil
}

func streamRows(db *sql.DB, table string, cols []colMeta, pkNames []string, fn func(pkKey, rowKey string)) error {
	if len(cols) == 0 {
		return nil
	}

	colIdx := make(map[string]int, len(cols))
	colList := make([]string, len(cols))
	for i, c := range cols {
		colIdx[c.Name] = i
		colList[i] = quoteIdent(c.Name)
	}

	pkPositions := make([]int, len(pkNames))
	for i, pk := range pkNames {
		pkPositions[i] = colIdx[pk]
	}

	// Sort columns by name when building row fingerprint so column reordering
	// alone does not alter equality, while schema additions/removals still differ.
	sortedColIdx := make([]int, len(cols))
	for i := range cols {
		sortedColIdx[i] = i
	}
	sort.Slice(sortedColIdx, func(i, j int) bool {
		return cols[sortedColIdx[i]].Name < cols[sortedColIdx[j]].Name
	})

	query := fmt.Sprintf(`SELECT %s FROM %s`, strings.Join(colList, ", "), quoteIdent(table))
	rows, err := db.Query(query)
	if err != nil {
		return err
	}
	defer rows.Close()

	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}

	var pkBuf, rowBuf []byte
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}

		pkBuf = pkBuf[:0]
		for _, pos := range pkPositions {
			pkBuf = appendEncodedValue(pkBuf, vals[pos])
		}

		rowBuf = rowBuf[:0]
		for _, idx := range sortedColIdx {
			rowBuf = appendLengthPrefixedString(rowBuf, cols[idx].Name)
			rowBuf = appendEncodedValue(rowBuf, vals[idx])
		}

		fn(string(pkBuf), string(rowBuf))
	}
	return rows.Err()
}

func appendLengthPrefixedString(dst []byte, s string) []byte {
	var lenBuf [4]byte
	binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(s)))
	dst = append(dst, lenBuf[:]...)
	return append(dst, s...)
}

func appendEncodedValue(dst []byte, v any) []byte {
	switch val := v.(type) {
	case nil:
		return append(dst, 0x00)
	case int64:
		var buf [9]byte
		buf[0] = 0x01
		binary.LittleEndian.PutUint64(buf[1:], uint64(val))
		return append(dst, buf[:]...)
	case float64:
		var buf [9]byte
		buf[0] = 0x02
		binary.LittleEndian.PutUint64(buf[1:], math.Float64bits(val))
		return append(dst, buf[:]...)
	case bool:
		if val {
			return append(dst, 0x03, 0x01)
		}
		return append(dst, 0x03, 0x00)
	case string:
		dst = append(dst, 0x04)
		return appendLengthPrefixedString(dst, val)
	case []byte:
		var lenBuf [5]byte
		lenBuf[0] = 0x05
		binary.LittleEndian.PutUint32(lenBuf[1:], uint32(len(val)))
		dst = append(dst, lenBuf[:]...)
		return append(dst, val...)
	case time.Time:
		var buf [9]byte
		buf[0] = 0x06
		binary.LittleEndian.PutUint64(buf[1:], uint64(val.UnixNano()))
		return append(dst, buf[:]...)
	default:
		s := fmt.Sprintf("%T:%v", val, val)
		dst = append(dst, 0x07)
		return appendLengthPrefixedString(dst, s)
	}
}

func quoteIdent(ident string) string {
	return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"`
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
