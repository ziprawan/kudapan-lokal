package bsv

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
)

// Magic is the 2-byte signature at the start of BSV files.
var Magic = [2]byte{0xBF, 0x11}

// SchemaType identifies the binary encoding of a BSV column.
type SchemaType uint8

const (
	TypeUShort    SchemaType = 16 // 0x10 - VLQ integer
	TypeUInt      SchemaType = 17 // 0x11 - VLQ integer
	TypeULong     SchemaType = 18 // 0x12 - VLQ integer
	TypeUInt64    SchemaType = 19 // 0x13 - fixed 8-byte big-endian unsigned integer
	TypeUNum      SchemaType = 32 // 0x20 - VLQ integer
	TypeUNumFixed SchemaType = 33 // 0x21 - fixed-width big-endian integer
	TypeBlob      SchemaType = 48 // 0x30 - VLQ-length-prefixed bytes
	TypeBlobFixed SchemaType = 49 // 0x31 - fixed-length bytes
	TypeText      SchemaType = 64 // 0x40 - NUL-terminated UTF-8 string
	TypeTextFixed SchemaType = 65 // 0x41 - fixed-length UTF-8 string
)

// String returns the human-readable schema type name.
func (t SchemaType) String() string {
	switch t {
	case TypeUShort:
		return "UShort"
	case TypeUInt:
		return "UInt"
	case TypeULong:
		return "ULong"
	case TypeUInt64:
		return "UInt64"
	case TypeUNum:
		return "UNum"
	case TypeUNumFixed:
		return "UNumFixed"
	case TypeBlob:
		return "Blob"
	case TypeBlobFixed:
		return "BlobFixed"
	case TypeText:
		return "Text"
	case TypeTextFixed:
		return "TextFixed"
	default:
		return fmt.Sprintf("Unknown(0x%02x)", uint8(t))
	}
}

// IsValid reports whether t is a recognized BSV schema type.
func (t SchemaType) IsValid() bool {
	switch t {
	case TypeUShort, TypeUInt, TypeULong, TypeUInt64, TypeUNum, TypeUNumFixed,
		TypeBlob, TypeBlobFixed, TypeText, TypeTextFixed:
		return true
	default:
		return false
	}
}

// HasFixedSize reports whether the schema definition includes a VLQ FixedSize.
func (t SchemaType) HasFixedSize() bool {
	return t == TypeUNumFixed || t == TypeBlobFixed || t == TypeTextFixed
}

// ColumnSchema describes a single column in the BSV header.
type ColumnSchema struct {
	Type      SchemaType
	FixedSize uint64
}

// Header contains the parsed metadata and column definitions from a BSV header.
type Header struct {
	HeaderSize    uint16
	RowCount      uint64
	MaxRowSize    uint64
	SchemaVersion uint64
	Schemas       []ColumnSchema
}

// ValueKind indicates the Go storage category of a parsed Value.
type ValueKind uint8

const (
	KindUint ValueKind = iota
	KindString
	KindBytes
)

// Value represents a single decoded BSV cell.
type Value struct {
	kind      ValueKind
	colType   SchemaType
	fixedSize uint64
	uintVal   uint64
	strVal    string
	bytesVal  []byte
}

// Kind returns the storage kind of the value.
func (v Value) Kind() ValueKind { return v.kind }

// Type returns the schema type of the column this value was read from.
func (v Value) Type() SchemaType { return v.colType }

// FixedSize returns the schema FixedSize for fixed-width column types.
func (v Value) FixedSize() uint64 { return v.fixedSize }

// AsString returns the string value for text columns.
func (v Value) AsString() (string, error) {
	if v.kind != KindString {
		return "", fmt.Errorf("bsv value is %s, not text", v.colType)
	}
	return v.strVal, nil
}

// AsUint64 returns the raw unsigned integer value for numeric columns.
func (v Value) AsUint64() (uint64, error) {
	if v.kind != KindUint {
		return 0, fmt.Errorf("bsv value is %s, not numeric", v.colType)
	}
	return v.uintVal, nil
}

// AsInt64 returns the signed int64 value for numeric columns (converting
// 8-byte UNumFixed values using two's complement, matching C# (long) casts).
func (v Value) AsInt64() (int64, error) {
	if v.kind != KindUint {
		return 0, fmt.Errorf("bsv value is %s, not numeric", v.colType)
	}
	if v.colType == TypeUNumFixed && v.fixedSize == 8 {
		return int64(v.uintVal), nil
	}
	if v.uintVal > math.MaxInt64 {
		return 0, fmt.Errorf("bsv value %d overflows int64", v.uintVal)
	}
	return int64(v.uintVal), nil
}

// AsBytes returns a copy of the byte slice for blob columns.
func (v Value) AsBytes() ([]byte, error) {
	if v.kind != KindBytes {
		return nil, fmt.Errorf("bsv value is %s, not blob", v.colType)
	}
	out := make([]byte, len(v.bytesVal))
	copy(out, v.bytesVal)
	return out, nil
}

// Raw returns the native Go value for v. For 8-byte UNumFixed columns it
// returns int64 (two's complement) to match manifest JSON representations.
func (v Value) Raw() any {
	switch v.kind {
	case KindString:
		return v.strVal
	case KindBytes:
		out := make([]byte, len(v.bytesVal))
		copy(out, v.bytesVal)
		return out
	case KindUint:
		if v.colType == TypeUNumFixed && v.fixedSize == 8 {
			return int64(v.uintVal)
		}
		return v.uintVal
	default:
		return nil
	}
}

// MarshalJSON serializes v to JSON using its Raw representation.
func (v Value) MarshalJSON() ([]byte, error) {
	return json.Marshal(v.Raw())
}

// Row holds the ordered column values for a single BSV record.
type Row []Value

// AnySlice returns the row values converted via Value.Raw.
func (r Row) AnySlice() []any {
	out := make([]any, len(r))
	for i, v := range r {
		out[i] = v.Raw()
	}
	return out
}

// MarshalJSON serializes a Row as a JSON array.
func (r Row) MarshalJSON() ([]byte, error) {
	if len(r) == 0 {
		return []byte("[]"), nil
	}
	return json.Marshal([]Value(r))
}

// Table represents a complete parsed BSV file (header + rows).
type Table struct {
	Header Header
	Rows   []Row
}

// MarshalJSON serializes Table rows as a 2D JSON array.
func (t Table) MarshalJSON() ([]byte, error) {
	if len(t.Rows) == 0 {
		return []byte("[]"), nil
	}
	return json.Marshal(t.Rows)
}

// Reader streams rows sequentially from an AnonymousSchema BSV payload.
type Reader struct {
	buf        []byte
	header     Header
	rowOffset  int
	rowsRead   uint64
	cachedPlan *structPlan
}

// NewReader validates the BSV magic and header of data and returns a Reader
// positioned at the first row (offset 4 + HeaderSize).
func NewReader(data []byte) (*Reader, error) {
	if len(data) < 4 {
		return nil, fmt.Errorf("bsv data too short (%d bytes): %w", len(data), io.ErrUnexpectedEOF)
	}
	if data[0] != Magic[0] || data[1] != Magic[1] {
		return nil, fmt.Errorf("invalid bsv magic 0x%02x 0x%02x, want 0x%02x 0x%02x", data[0], data[1], Magic[0], Magic[1])
	}

	headerSize := binary.BigEndian.Uint16(data[2:4])
	if int(headerSize) > len(data)-4 {
		return nil, fmt.Errorf("bsv header size %d exceeds payload (%d remaining bytes): %w", headerSize, len(data)-4, io.ErrUnexpectedEOF)
	}

	hdrBuf := data[4 : 4+int(headerSize)]
	hdr, err := parseHeader(hdrBuf, headerSize, len(data))
	if err != nil {
		return nil, err
	}

	return &Reader{
		buf:       data,
		header:    hdr,
		rowOffset: 4 + int(headerSize),
	}, nil
}

func parseHeader(hdrBuf []byte, headerSize uint16, totalLen int) (Header, error) {
	var hdr Header
	hdr.HeaderSize = headerSize
	off := 0

	var err error
	if hdr.RowCount, err = ReadVLQ(hdrBuf, &off, maxVLQBytes); err != nil {
		return hdr, fmt.Errorf("read bsv row_count: %w", err)
	}
	if hdr.MaxRowSize, err = ReadVLQ(hdrBuf, &off, maxVLQBytes); err != nil {
		return hdr, fmt.Errorf("read bsv max_row_size: %w", err)
	}
	if hdr.SchemaVersion, err = ReadVLQ(hdrBuf, &off, maxVLQBytes); err != nil {
		return hdr, fmt.Errorf("read bsv schema_version: %w", err)
	}
	schemaCount, err := ReadVLQ(hdrBuf, &off, maxVLQBytes)
	if err != nil {
		return hdr, fmt.Errorf("read bsv schema_count: %w", err)
	}
	if schemaCount > uint64(len(hdrBuf)-off) {
		return hdr, fmt.Errorf("bsv schema_count %d exceeds remaining header bytes (%d)", schemaCount, len(hdrBuf)-off)
	}

	if schemaCount > 0 {
		hdr.Schemas = make([]ColumnSchema, int(schemaCount))
		for i := 0; i < int(schemaCount); i++ {
			if off >= len(hdrBuf) {
				return hdr, fmt.Errorf("bsv schema column %d truncated: %w", i, io.ErrUnexpectedEOF)
			}
			colType := SchemaType(hdrBuf[off])
			off++
			if !colType.IsValid() {
				return hdr, fmt.Errorf("unknown bsv schema type %s at column %d", colType, i)
			}

			var fixedSize uint64
			if colType.HasFixedSize() {
				fixedSize, err = ReadVLQ(hdrBuf, &off, maxVLQBytes)
				if err != nil {
					return hdr, fmt.Errorf("read fixed_size for column %d (%s): %w", i, colType, err)
				}
				if colType == TypeUNumFixed && (fixedSize < 1 || fixedSize > 8) {
					return hdr, fmt.Errorf("invalid UNumFixed byte width %d at column %d: want 1..8", fixedSize, i)
				}
				if (colType == TypeBlobFixed || colType == TypeTextFixed) && fixedSize > uint64(totalLen) {
					return hdr, fmt.Errorf("fixed_size %d at column %d exceeds file length %d", fixedSize, i, totalLen)
				}
			}

			hdr.Schemas[i] = ColumnSchema{
				Type:      colType,
				FixedSize: fixedSize,
			}
		}
	}

	return hdr, nil
}

// Header returns a copy of the parsed BSV header.
func (r *Reader) Header() Header {
	schemas := make([]ColumnSchema, len(r.header.Schemas))
	copy(schemas, r.header.Schemas)
	h := r.header
	h.Schemas = schemas
	return h
}

// RowCount returns the expected number of rows declared in the header.
func (r *Reader) RowCount() uint64 {
	return r.header.RowCount
}

// Next reads the next row according to the parsed header schema.
// When all rows have been read, it verifies no trailing bytes remain and returns (nil, false, nil).
func (r *Reader) Next() (Row, bool, error) {
	if r.rowsRead >= r.header.RowCount {
		if r.rowOffset != len(r.buf) {
			return nil, false, fmt.Errorf("bsv has %d trailing bytes after %d rows", len(r.buf)-r.rowOffset, r.header.RowCount)
		}
		return nil, false, nil
	}
	if len(r.header.Schemas) == 0 {
		return nil, false, errors.New("bsv header contains no column schemas")
	}

	row := make(Row, len(r.header.Schemas))
	for i, col := range r.header.Schemas {
		val, err := readColumnValue(r.buf, &r.rowOffset, col)
		if err != nil {
			return nil, false, fmt.Errorf("row %d col %d (%s): %w", r.rowsRead, i, col.Type, err)
		}
		row[i] = val
	}
	r.rowsRead++
	return row, true, nil
}

// ReadAll reads all remaining rows and returns them as a slice.
func (r *Reader) ReadAll() ([]Row, error) {
	remaining := r.header.RowCount - r.rowsRead
	capHint := int(remaining)
	if remaining > 4096 {
		capHint = 4096
	}
	rows := make([]Row, 0, capHint)
	for {
		row, ok, err := r.Next()
		if err != nil {
			return nil, err
		}
		if !ok {
			return rows, nil
		}
		rows = append(rows, row)
	}
}

func readColumnValue(buf []byte, offset *int, col ColumnSchema) (Value, error) {
	switch col.Type {
	case TypeUShort, TypeUInt, TypeULong, TypeUNum:
		v, err := ReadVLQ(buf, offset, maxVLQBytes)
		if err != nil {
			return Value{}, err
		}
		return Value{
			kind:    KindUint,
			colType: col.Type,
			uintVal: v,
		}, nil

	case TypeUInt64:
		v, err := ReadUNum(buf, offset, 8)
		if err != nil {
			return Value{}, err
		}
		return Value{
			kind:      KindUint,
			colType:   col.Type,
			fixedSize: 8,
			uintVal:   v,
		}, nil

	case TypeUNumFixed:
		v, err := ReadUNum(buf, offset, int(col.FixedSize))
		if err != nil {
			return Value{}, err
		}
		return Value{
			kind:      KindUint,
			colType:   col.Type,
			fixedSize: col.FixedSize,
			uintVal:   v,
		}, nil

	case TypeText:
		s, err := ReadText(buf, offset)
		if err != nil {
			return Value{}, err
		}
		return Value{
			kind:    KindString,
			colType: col.Type,
			strVal:  s,
		}, nil

	case TypeTextFixed:
		s, err := ReadFixedText(buf, offset, int(col.FixedSize))
		if err != nil {
			return Value{}, err
		}
		return Value{
			kind:      KindString,
			colType:   col.Type,
			fixedSize: col.FixedSize,
			strVal:    s,
		}, nil

	case TypeBlob:
		b, err := ReadBlob(buf, offset)
		if err != nil {
			return Value{}, err
		}
		return Value{
			kind:     KindBytes,
			colType:  col.Type,
			bytesVal: b,
		}, nil

	case TypeBlobFixed:
		b, err := ReadFixedBlob(buf, offset, int(col.FixedSize))
		if err != nil {
			return Value{}, err
		}
		return Value{
			kind:      KindBytes,
			colType:   col.Type,
			fixedSize: col.FixedSize,
			bytesVal:  b,
		}, nil

	default:
		return Value{}, fmt.Errorf("unsupported schema type %s", col.Type)
	}
}

// Parse parses a complete BSV byte buffer into a Table.
func Parse(data []byte) (*Table, error) {
	r, err := NewReader(data)
	if err != nil {
		return nil, err
	}
	rows, err := r.ReadAll()
	if err != nil {
		return nil, err
	}
	return &Table{
		Header: r.Header(),
		Rows:   rows,
	}, nil
}

// ParseReader reads all bytes from rd and parses them into a Table.
func ParseReader(rd io.Reader) (*Table, error) {
	data, err := io.ReadAll(rd)
	if err != nil {
		return nil, fmt.Errorf("read bsv stream: %w", err)
	}
	return Parse(data)
}

// ParseFile reads the file at path and parses it into a Table.
func ParseFile(path string) (*Table, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read bsv file %q: %w", path, err)
	}
	return Parse(data)
}
