package bsv

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type rootManifestRow struct {
	Platform string
	Size     uint64
	Checksum int64
}

func TestParseRootManifestFixture(t *testing.T) {
	bsvPath := filepath.Join("testdata", "root.manifest.bsv")
	jsonPath := filepath.Join("testdata", "root.manifest.json")

	table, err := ParseFile(bsvPath)
	if err != nil {
		t.Fatalf("ParseFile(%q) error = %v", bsvPath, err)
	}

	if table.Header.HeaderSize != 8 {
		t.Errorf("HeaderSize = %d, want 8", table.Header.HeaderSize)
	}
	if table.Header.RowCount != 3 {
		t.Errorf("RowCount = %d, want 3", table.Header.RowCount)
	}
	if table.Header.MaxRowSize != 24 {
		t.Errorf("MaxRowSize = %d, want 24", table.Header.MaxRowSize)
	}
	if table.Header.SchemaVersion != 32 {
		t.Errorf("SchemaVersion = %d, want 32", table.Header.SchemaVersion)
	}
	wantSchemas := []ColumnSchema{
		{Type: TypeText},
		{Type: TypeULong},
		{Type: TypeUNumFixed, FixedSize: 8},
	}
	if !reflect.DeepEqual(table.Header.Schemas, wantSchemas) {
		t.Fatalf("Schemas = %+v, want %+v", table.Header.Schemas, wantSchemas)
	}

	wantRows := []rootManifestRow{
		{Platform: "Windows", Size: 684, Checksum: -1287387911940158962},
		{Platform: "iOS", Size: 684, Checksum: 8009544546994281154},
		{Platform: "Android", Size: 684, Checksum: -1204404390377947589},
	}
	if len(table.Rows) != len(wantRows) {
		t.Fatalf("len(Rows) = %d, want %d", len(table.Rows), len(wantRows))
	}
	for i, want := range wantRows {
		p, err := table.Rows[i][0].AsString()
		if err != nil {
			t.Fatalf("row %d col 0 AsString error = %v", i, err)
		}
		sz, err := table.Rows[i][1].AsUint64()
		if err != nil {
			t.Fatalf("row %d col 1 AsUint64 error = %v", i, err)
		}
		cs, err := table.Rows[i][2].AsInt64()
		if err != nil {
			t.Fatalf("row %d col 2 AsInt64 error = %v", i, err)
		}
		csUint, err := table.Rows[i][2].AsUint64()
		if err != nil {
			t.Fatalf("row %d col 2 AsUint64 error = %v", i, err)
		}
		if p != want.Platform || sz != want.Size || cs != want.Checksum || int64(csUint) != want.Checksum {
			t.Errorf("row %d = (%q, %d, %d), want (%q, %d, %d)", i, p, sz, cs, want.Platform, want.Size, want.Checksum)
		}
	}

	gotJSONBytes, err := json.Marshal(table)
	if err != nil {
		t.Fatalf("json.Marshal(table) error = %v", err)
	}
	wantJSONBytes, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", jsonPath, err)
	}

	var gotJSON, wantJSON any
	if err := json.Unmarshal(gotJSONBytes, &gotJSON); err != nil {
		t.Fatalf("unmarshal gotJSON error = %v", err)
	}
	if err := json.Unmarshal(wantJSONBytes, &wantJSON); err != nil {
		t.Fatalf("unmarshal wantJSON error = %v", err)
	}
	if !reflect.DeepEqual(gotJSON, wantJSON) {
		t.Errorf("JSON mismatch:\ngot:  %s\nwant: %s", gotJSONBytes, wantJSONBytes)
	}
}

func TestDecodeStructRootManifest(t *testing.T) {
	bsvPath := filepath.Join("testdata", "root.manifest.bsv")
	data, err := os.ReadFile(bsvPath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", bsvPath, err)
	}

	want := []rootManifestRow{
		{Platform: "Windows", Size: 684, Checksum: -1287387911940158962},
		{Platform: "iOS", Size: 684, Checksum: 8009544546994281154},
		{Platform: "Android", Size: 684, Checksum: -1204404390377947589},
	}

	got, err := DecodeFile[rootManifestRow](bsvPath)
	if err != nil {
		t.Fatalf("DecodeFile() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DecodeFile() = %+v, want %+v", got, want)
	}

	var ptrRows []*rootManifestRow
	if err := Unmarshal(data, &ptrRows); err != nil {
		t.Fatalf("Unmarshal(*[]*T) error = %v", err)
	}
	if len(ptrRows) != 3 || *ptrRows[0] != want[0] || *ptrRows[2] != want[2] {
		t.Errorf("Unmarshal(*[]*T) = %+v", ptrRows)
	}

	// Verify explicit index reordering, ignored derived field, and type/size tag checks.
	type reorderedRow struct {
		Ignored  string `bsv:"-"`
		Checksum uint64 `bsv:"2,type=unumfixed,size=8"`
		Platform string `bsv:"0,type=text"`
		Size     uint64 `bsv:"1,type=ulong"`
	}
	reordered, err := Decode[reorderedRow](data)
	if err != nil {
		t.Fatalf("Decode[reorderedRow]() error = %v", err)
	}
	if len(reordered) != 3 {
		t.Fatalf("len(reordered) = %d, want 3", len(reordered))
	}
	if reordered[0].Platform != "Windows" || reordered[0].Size != 684 || int64(reordered[0].Checksum) != want[0].Checksum {
		t.Errorf("reordered[0] = %+v", reordered[0])
	}

	// Verify streaming Reader.NextInto.
	r, err := NewReader(data)
	if err != nil {
		t.Fatalf("NewReader() error = %v", err)
	}
	var streamed []rootManifestRow
	for {
		var row rootManifestRow
		ok, err := r.NextInto(&row)
		if err != nil {
			t.Fatalf("NextInto() error = %v", err)
		}
		if !ok {
			break
		}
		streamed = append(streamed, row)
	}
	if !reflect.DeepEqual(streamed, want) {
		t.Errorf("NextInto() = %+v, want %+v", streamed, want)
	}

	// Verify Row.Scan.
	table, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	var scanned rootManifestRow
	if err := table.Rows[1].Scan(table.Header.Schemas, &scanned); err != nil {
		t.Fatalf("Row.Scan() error = %v", err)
	}
	if scanned != want[1] {
		t.Errorf("Row.Scan() = %+v, want %+v", scanned, want[1])
	}
}

func TestStructMapperSchemaValidation(t *testing.T) {
	bsvPath := filepath.Join("testdata", "root.manifest.bsv")
	data, err := os.ReadFile(bsvPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	t.Run("missing_column", func(t *testing.T) {
		type tooFew struct {
			Platform string
			Size     uint64
		}
		if _, err := Decode[tooFew](data); err == nil {
			t.Fatal("expected error for too few struct fields")
		}
	})

	t.Run("extra_column", func(t *testing.T) {
		type tooMany struct {
			Platform string
			Size     uint64
			Checksum int64
			Extra    string
		}
		if _, err := Decode[tooMany](data); err == nil {
			t.Fatal("expected error for extra struct field")
		}
	})

	t.Run("wrong_field_types", func(t *testing.T) {
		type badText struct {
			Platform []byte
			Size     uint64
			Checksum int64
		}
		if _, err := Decode[badText](data); err == nil {
			t.Fatal("expected error for []byte mapped to Text")
		}

		type narrowChecksum struct {
			Platform string
			Size     uint64
			Checksum int32
		}
		if _, err := Decode[narrowChecksum](data); err == nil {
			t.Fatal("expected error for int32 mapped to UNumFixed(8)")
		}

		type narrowSize struct {
			Platform string
			Size     uint32
			Checksum int64
		}
		if _, err := Decode[narrowSize](data); err == nil {
			t.Fatal("expected error for uint32 mapped to ULong")
		}
	})

	t.Run("tag_validation_errors", func(t *testing.T) {
		type dupCol struct {
			A string `bsv:"0"`
			B uint64 `bsv:"0"`
			C int64  `bsv:"2"`
		}
		if _, err := Decode[dupCol](data); err == nil {
			t.Fatal("expected error for duplicate column index")
		}

		type wrongTagType struct {
			Platform string `bsv:",type=blob"`
			Size     uint64
			Checksum int64
		}
		if _, err := Decode[wrongTagType](data); err == nil {
			t.Fatal("expected error for mismatched bsv tag type")
		}

		type wrongTagSize struct {
			Platform string
			Size     uint64
			Checksum int64 `bsv:",type=unumfixed,size=4"`
		}
		if _, err := Decode[wrongTagSize](data); err == nil {
			t.Fatal("expected error for mismatched bsv tag fixed size")
		}
	})

	t.Run("zero_row_bsv_still_validates_schema", func(t *testing.T) {
		emptyBSV := buildTestBSV(0, 16, 1, []ColumnSchema{
			{Type: TypeText},
			{Type: TypeUShort},
		}, nil)
		type badStruct struct {
			Name string
			Val  int16 // int16 cannot hold full uint16 range
		}
		if _, err := Decode[badStruct](emptyBSV); err == nil {
			t.Fatal("expected error when int16 is mapped to UShort")
		}
	})

	t.Run("blob_fixed_array_length_check", func(t *testing.T) {
		blobBSV := buildTestBSV(1, 4, 1, []ColumnSchema{
			{Type: TypeBlobFixed, FixedSize: 4},
		}, []byte{1, 2, 3, 4})

		type badArrayLen struct {
			Hash [3]byte
		}
		if _, err := Decode[badArrayLen](blobBSV); err == nil {
			t.Fatal("expected error for [3]byte mapped to BlobFixed(4)")
		}

		type okArray struct {
			Hash [4]byte
		}
		got, err := Decode[okArray](blobBSV)
		if err != nil {
			t.Fatalf("Decode[okArray]() error = %v", err)
		}
		if got[0].Hash != [4]byte{1, 2, 3, 4} {
			t.Errorf("got[0].Hash = %v, want [1 2 3 4]", got[0].Hash)
		}
	})

	t.Run("invalid_destinations", func(t *testing.T) {
		var s []rootManifestRow
		if err := Unmarshal(data, s); err == nil {
			t.Fatal("expected error for non-pointer slice")
		}
		var notSlice int
		if err := Unmarshal(data, &notSlice); err == nil {
			t.Fatal("expected error for pointer to non-slice")
		}
		var ints []int
		if err := Unmarshal(data, &ints); err == nil {
			t.Fatal("expected error for slice of non-struct")
		}
		r, err := NewReader(data)
		if err != nil {
			t.Fatalf("NewReader() error = %v", err)
		}
		if _, err := r.NextInto(nil); err == nil {
			t.Fatal("expected error for NextInto(nil)")
		}
	})
}

func TestReaderStreamingNext(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "root.manifest.bsv"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	r, err := NewReader(data)
	if err != nil {
		t.Fatalf("NewReader() error = %v", err)
	}
	if r.RowCount() != 3 {
		t.Fatalf("RowCount() = %d, want 3", r.RowCount())
	}

	count := 0
	for {
		row, ok, err := r.Next()
		if err != nil {
			t.Fatalf("Next() error = %v", err)
		}
		if !ok {
			break
		}
		if len(row) != 3 {
			t.Fatalf("row %d len = %d, want 3", count, len(row))
		}
		count++
	}
	if count != 3 {
		t.Fatalf("streamed %d rows, want 3", count)
	}
}

func TestAllSchemaTypes(t *testing.T) {
	schemas := []ColumnSchema{
		{Type: TypeUShort},
		{Type: TypeUInt},
		{Type: TypeULong},
		{Type: TypeUInt64},
		{Type: TypeUNum},
		{Type: TypeUNumFixed, FixedSize: 4},
		{Type: TypeBlob},
		{Type: TypeBlobFixed, FixedSize: 3},
		{Type: TypeText},
		{Type: TypeTextFixed, FixedSize: 4},
	}

	var rowBytes bytes.Buffer
	rowBytes.Write(encodeVLQ(65535))                                     // UShort
	rowBytes.Write(encodeVLQ(0x12345678))                                // UInt
	rowBytes.Write(encodeVLQ(1 << 40))                                   // ULong
	rowBytes.Write([]byte{0xc6, 0x47, 0xc2, 0xe2, 0xd3, 0x95, 0x15, 0xa1}) // UInt64 (0x13)
	rowBytes.Write(encodeVLQ(42))                                        // UNum
	rowBytes.Write([]byte{0x11, 0x22, 0x33, 0x44})                       // UNumFixed(4)
	rowBytes.Write(encodeVLQ(2))                                         // Blob len
	rowBytes.Write([]byte{0xaa, 0xbb})                                   // Blob data
	rowBytes.Write([]byte{0x01, 0x02, 0x03})                             // BlobFixed(3)
	rowBytes.WriteString("uma\x00")                                      // Text
	rowBytes.WriteString("ABCD")                                         // TextFixed(4)

	raw := buildTestBSV(1, uint64(rowBytes.Len()), 7, schemas, rowBytes.Bytes())

	type allTypesRow struct {
		A uint16
		B uint32
		C uint64
		X uint64 `bsv:",type=uint64"`
		D uint64
		E uint32
		F []byte
		G [3]byte
		H string
		I string
	}

	rows, err := Decode[allTypesRow](raw)
	if err != nil {
		t.Fatalf("Decode[allTypesRow]() error = %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	got := rows[0]
	if got.A != 65535 || got.B != 0x12345678 || got.C != (1<<40) || got.X != 0xc647c2e2d39515a1 || got.D != 42 || got.E != 0x11223344 {
		t.Errorf("numeric mismatch: %+v", got)
	}
	if !bytes.Equal(got.F, []byte{0xaa, 0xbb}) || got.G != [3]byte{1, 2, 3} || got.H != "uma" || got.I != "ABCD" {
		t.Errorf("text/blob mismatch: %+v", got)
	}

	// Verify 7-column asset manifest layout (matching live master manifest).
	masterHex := []byte{
		0xbf, 0x11, 0x00, 0x0c, 0x01, 0x30, 0x11, 0x07,
		0x40, 0x40, 0x11, 0x11, 0x12, 0x21, 0x08, 0x13,
	}
	masterRow := append(masterHex, []byte("master.mdb.lz4\x00\x00\x00\x00")...)
	masterRow = append(masterRow, 0x82, 0xdb, 0xb9, 0x32)
	masterRow = append(masterRow, 0x0c, 0x85, 0x55, 0x9c, 0x28, 0xc3, 0x08, 0x38)
	masterRow = append(masterRow, 0, 0, 0, 0, 0, 0, 0, 0)

	type masterManifestRow struct {
		Name     string
		Deps     string
		Group    uint32
		Priority uint32
		Size     int64
		Checksum int64
		Extra    uint64
	}
	mRows, err := Decode[masterManifestRow](masterRow)
	if err != nil {
		t.Fatalf("Decode[masterManifestRow]() error = %v", err)
	}
	if len(mRows) != 1 || mRows[0].Name != "master.mdb.lz4" || mRows[0].Size != 5692594 || mRows[0].Checksum != 0x0c85559c28c30838 {
		t.Errorf("unexpected masterManifestRow: %+v", mRows)
	}

	// Verify 0-row table returns empty slice without error.
	zeroRaw := buildTestBSV(0, 0, 1, schemas, nil)
	table, err := ParseReader(bytes.NewReader(zeroRaw))
	if err != nil {
		t.Fatalf("ParseReader(0 rows) error = %v", err)
	}
	if len(table.Rows) != 0 {
		t.Fatalf("len(Rows) = %d, want 0", len(table.Rows))
	}
	jb, err := json.Marshal(table)
	if err != nil || string(jb) != "[]" {
		t.Errorf("json.Marshal(empty table) = %q, err=%v; want []", jb, err)
	}
}

func TestMalformedInputs(t *testing.T) {
	t.Run("bad_magic", func(t *testing.T) {
		if _, err := Parse([]byte{0x00, 0x00, 0x00, 0x04, 0, 0, 0, 0}); err == nil {
			t.Fatal("expected error for invalid magic")
		}
	})

	t.Run("header_exceeds_eof", func(t *testing.T) {
		if _, err := Parse([]byte{0xBF, 0x11, 0x00, 0x10, 0x01}); err == nil {
			t.Fatal("expected error for headerSize past EOF")
		}
	})

	t.Run("unknown_schema_type", func(t *testing.T) {
		data := []byte{0xBF, 0x11, 0x00, 0x05, 0x00, 0x00, 0x01, 0x01, 0x99}
		if _, err := Parse(data); err == nil {
			t.Fatal("expected error for unknown schema type")
		}
	})

	t.Run("invalid_unum_fixed_size", func(t *testing.T) {
		data0 := []byte{0xBF, 0x11, 0x00, 0x06, 0x00, 0x00, 0x01, 0x01, byte(TypeUNumFixed), 0x00}
		if _, err := Parse(data0); err == nil {
			t.Fatal("expected error for UNumFixed size 0")
		}
		data9 := []byte{0xBF, 0x11, 0x00, 0x06, 0x00, 0x00, 0x01, 0x01, byte(TypeUNumFixed), 0x09}
		if _, err := Parse(data9); err == nil {
			t.Fatal("expected error for UNumFixed size 9")
		}
	})

	t.Run("unterminated_text", func(t *testing.T) {
		raw := buildTestBSV(1, 3, 1, []ColumnSchema{{Type: TypeText}}, []byte("abc"))
		if _, err := Parse(raw); err == nil {
			t.Fatal("expected error for unterminated text")
		}
	})

	t.Run("trailing_bytes", func(t *testing.T) {
		raw := buildTestBSV(1, 2, 1, []ColumnSchema{{Type: TypeText}}, []byte("a\x00extra"))
		if _, err := Parse(raw); err == nil {
			t.Fatal("expected error for trailing bytes after rowCount")
		}
	})

	t.Run("schemaless_non_empty", func(t *testing.T) {
		raw := buildTestBSV(1, 1, 1, nil, []byte{0x00})
		if _, err := Parse(raw); err == nil {
			t.Fatal("expected error for schema-less BSV with rowCount > 0")
		}
	})

	t.Run("truncated_vlq", func(t *testing.T) {
		off := 0
		if _, err := ReadVLQ([]byte{0x85}, &off, 8); err == nil {
			t.Fatal("expected error for truncated VLQ")
		}
	})
}

func buildTestBSV(rowCount, maxRowSize, schemaVersion uint64, schemas []ColumnSchema, rowsData []byte) []byte {
	var hdr bytes.Buffer
	hdr.Write(encodeVLQ(rowCount))
	hdr.Write(encodeVLQ(maxRowSize))
	hdr.Write(encodeVLQ(schemaVersion))
	hdr.Write(encodeVLQ(uint64(len(schemas))))
	for _, s := range schemas {
		hdr.WriteByte(byte(s.Type))
		if s.Type.HasFixedSize() {
			hdr.Write(encodeVLQ(s.FixedSize))
		}
	}

	var out bytes.Buffer
	out.Write(Magic[:])
	var sz [2]byte
	binary.BigEndian.PutUint16(sz[:], uint16(hdr.Len()))
	out.Write(sz[:])
	out.Write(hdr.Bytes())
	out.Write(rowsData)
	return out.Bytes()
}

func encodeVLQ(v uint64) []byte {
	var tmp [10]byte
	i := len(tmp) - 1
	tmp[i] = byte(v & 0x7f)
	v >>= 7
	for v > 0 {
		i--
		tmp[i] = byte(v&0x7f) | 0x80
		v >>= 7
	}
	return append([]byte(nil), tmp[i:]...)
}
