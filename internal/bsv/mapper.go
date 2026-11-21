package bsv

import (
	"errors"
	"fmt"
	"math"
	"os"
	"reflect"
	"strconv"
	"strings"
)

type fieldBinding struct {
	fieldIndex []int
	colIndex   int
	fieldName  string
	col        ColumnSchema
}

type structPlan struct {
	structType reflect.Type
	bindings   []fieldBinding
}

type fieldTagSpec struct {
	skip        bool
	hasColIndex bool
	colIndex    int
	hasType     bool
	schemaType  SchemaType
	hasSize     bool
	fixedSize   uint64
}

// Unmarshal parses BSV data and decodes all rows into dst, which must be a
// pointer to a slice of structs or struct pointers (*[]T or *[]*T).
// It validates struct compatibility against the BSV header schema before decoding rows.
func Unmarshal(data []byte, dst any) error {
	if dst == nil {
		return errors.New("bsv unmarshal destination cannot be nil")
	}
	dstVal := reflect.ValueOf(dst)
	if dstVal.Kind() != reflect.Pointer || dstVal.IsNil() {
		return errors.New("bsv unmarshal destination must be a non-nil pointer to a slice")
	}
	sliceVal := dstVal.Elem()
	if sliceVal.Kind() != reflect.Slice {
		return fmt.Errorf("bsv unmarshal destination must point to a slice, got %s", sliceVal.Type())
	}

	elemType := sliceVal.Type().Elem()
	elemIsPtr := false
	structType := elemType
	if elemType.Kind() == reflect.Pointer {
		elemIsPtr = true
		structType = elemType.Elem()
	}
	if structType.Kind() != reflect.Struct {
		return fmt.Errorf("bsv unmarshal slice element must be a struct or struct pointer, got %s", elemType)
	}

	r, err := NewReader(data)
	if err != nil {
		return err
	}
	plan, err := compileStructPlan(structType, r.header.Schemas)
	if err != nil {
		return err
	}

	remaining := r.header.RowCount - r.rowsRead
	capHint := int(remaining)
	if remaining > 4096 {
		capHint = 4096
	}
	out := reflect.MakeSlice(sliceVal.Type(), 0, capHint)

	for {
		row, ok, err := r.Next()
		if err != nil {
			return err
		}
		if !ok {
			break
		}

		structPtr := reflect.New(structType)
		if err := applyRowToStruct(row, plan, structPtr.Elem()); err != nil {
			return fmt.Errorf("row %d: %w", out.Len(), err)
		}
		if elemIsPtr {
			out = reflect.Append(out, structPtr)
		} else {
			out = reflect.Append(out, structPtr.Elem())
		}
	}

	sliceVal.Set(out)
	return nil
}

// Decode parses BSV data into a slice of T structs, validating T against the BSV schema.
func Decode[T any](data []byte) ([]T, error) {
	var out []T
	if err := Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// DecodeFile reads the BSV file at path and decodes it into a slice of T structs.
func DecodeFile[T any](path string) ([]T, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read bsv file %q: %w", path, err)
	}
	return Decode[T](data)
}

// NextInto reads the next row from r and decodes it into the struct pointer dst,
// validating dst against the BSV schema.
func (r *Reader) NextInto(dst any) (bool, error) {
	dstElem, structType, err := validateStructPtr(dst)
	if err != nil {
		return false, err
	}
	if r.cachedPlan == nil || r.cachedPlan.structType != structType {
		plan, err := compileStructPlan(structType, r.header.Schemas)
		if err != nil {
			return false, err
		}
		r.cachedPlan = plan
	}

	row, ok, err := r.Next()
	if err != nil || !ok {
		return ok, err
	}
	if err := applyRowToStruct(row, r.cachedPlan, dstElem); err != nil {
		return false, fmt.Errorf("row %d: %w", r.rowsRead-1, err)
	}
	return true, nil
}

// Scan validates dst (a non-nil struct pointer) against schemas and maps the
// row values into dst's fields.
func (row Row) Scan(schemas []ColumnSchema, dst any) error {
	dstElem, structType, err := validateStructPtr(dst)
	if err != nil {
		return err
	}
	if len(row) != len(schemas) {
		return fmt.Errorf("bsv row has %d values, schema expects %d", len(row), len(schemas))
	}
	plan, err := compileStructPlan(structType, schemas)
	if err != nil {
		return err
	}
	return applyRowToStruct(row, plan, dstElem)
}

func validateStructPtr(dst any) (reflect.Value, reflect.Type, error) {
	if dst == nil {
		return reflect.Value{}, nil, errors.New("bsv destination struct pointer cannot be nil")
	}
	v := reflect.ValueOf(dst)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return reflect.Value{}, nil, errors.New("bsv destination must be a non-nil pointer to a struct")
	}
	elem := v.Elem()
	if elem.Kind() != reflect.Struct {
		return reflect.Value{}, nil, fmt.Errorf("bsv destination must point to a struct, got %s", elem.Type())
	}
	return elem, elem.Type(), nil
}

func compileStructPlan(structType reflect.Type, schemas []ColumnSchema) (*structPlan, error) {
	if len(schemas) == 0 {
		return nil, errors.New("bsv header contains no column schemas to validate struct against")
	}

	used := make([]bool, len(schemas))
	bindings := make([]fieldBinding, 0, len(schemas))
	nextAuto := 0

	for i := 0; i < structType.NumField(); i++ {
		f := structType.Field(i)
		if !f.IsExported() {
			continue
		}

		spec, err := parseFieldTag(f.Tag.Get("bsv"))
		if err != nil {
			return nil, fmt.Errorf("struct %s field %s: %w", structType.Name(), f.Name, err)
		}
		if spec.skip {
			continue
		}

		colIdx := nextAuto
		if spec.hasColIndex {
			colIdx = spec.colIndex
		} else {
			for colIdx < len(schemas) && used[colIdx] {
				colIdx++
			}
			nextAuto = colIdx + 1
		}

		if colIdx < 0 || colIdx >= len(schemas) {
			return nil, fmt.Errorf("struct %s field %s maps to column %d, but schema has %d columns (0..%d)",
				structType.Name(), f.Name, colIdx, len(schemas), len(schemas)-1)
		}
		if used[colIdx] {
			return nil, fmt.Errorf("struct %s field %s duplicates mapping for column %d",
				structType.Name(), f.Name, colIdx)
		}
		used[colIdx] = true

		col := schemas[colIdx]
		if err := validateFieldCompatibility(f, spec, colIdx, col); err != nil {
			return nil, fmt.Errorf("struct %s: %w", structType.Name(), err)
		}

		bindings = append(bindings, fieldBinding{
			fieldIndex: f.Index,
			colIndex:   colIdx,
			fieldName:  f.Name,
			col:        col,
		})
	}

	for colIdx, mapped := range used {
		if !mapped {
			return nil, fmt.Errorf("struct %s does not map schema column %d (%s)",
				structType.Name(), colIdx, schemas[colIdx].Type)
		}
	}

	return &structPlan{
		structType: structType,
		bindings:   bindings,
	}, nil
}

func parseFieldTag(tag string) (fieldTagSpec, error) {
	var spec fieldTagSpec
	if tag == "" {
		return spec, nil
	}
	if tag == "-" {
		spec.skip = true
		return spec, nil
	}

	parts := strings.Split(tag, ",")
	for idx, rawPart := range parts {
		part := strings.TrimSpace(rawPart)
		if part == "" {
			continue
		}
		if idx == 0 && !strings.Contains(part, "=") {
			colIdx, err := strconv.Atoi(part)
			if err != nil || colIdx < 0 {
				return spec, fmt.Errorf("invalid column index %q in bsv tag", part)
			}
			spec.hasColIndex = true
			spec.colIndex = colIdx
			continue
		}

		k, v, ok := strings.Cut(part, "=")
		if !ok {
			return spec, fmt.Errorf("invalid bsv tag option %q", part)
		}
		k = strings.ToLower(strings.TrimSpace(k))
		v = strings.ToLower(strings.TrimSpace(v))
		switch k {
		case "type":
			st, err := parseSchemaTypeName(v)
			if err != nil {
				return spec, err
			}
			spec.hasType = true
			spec.schemaType = st
		case "size":
			sz, err := strconv.ParseUint(v, 10, 64)
			if err != nil {
				return spec, fmt.Errorf("invalid size %q in bsv tag: %w", v, err)
			}
			spec.hasSize = true
			spec.fixedSize = sz
		default:
			return spec, fmt.Errorf("unknown bsv tag option %q", k)
		}
	}

	if spec.hasSize && spec.hasType && !spec.schemaType.HasFixedSize() {
		return spec, fmt.Errorf("bsv tag size option is only valid for fixed-size types, not %s", spec.schemaType)
	}
	return spec, nil
}

func parseSchemaTypeName(name string) (SchemaType, error) {
	switch name {
	case "ushort":
		return TypeUShort, nil
	case "uint":
		return TypeUInt, nil
	case "ulong":
		return TypeULong, nil
	case "uint64":
		return TypeUInt64, nil
	case "unum":
		return TypeUNum, nil
	case "unumfixed":
		return TypeUNumFixed, nil
	case "blob":
		return TypeBlob, nil
	case "blobfixed":
		return TypeBlobFixed, nil
	case "text":
		return TypeText, nil
	case "textfixed":
		return TypeTextFixed, nil
	default:
		return 0, fmt.Errorf("unknown schema type %q in bsv tag", name)
	}
}

func validateFieldCompatibility(f reflect.StructField, spec fieldTagSpec, colIdx int, col ColumnSchema) error {
	if spec.hasType && spec.schemaType != col.Type {
		return fmt.Errorf("field %s (col %d) tag requires %s, schema has %s", f.Name, colIdx, spec.schemaType, col.Type)
	}
	if spec.hasSize {
		if !col.Type.HasFixedSize() {
			return fmt.Errorf("field %s (col %d) tag requires fixed size %d, but schema type %s is variable-sized",
				f.Name, colIdx, spec.fixedSize, col.Type)
		}
		if spec.fixedSize != col.FixedSize {
			return fmt.Errorf("field %s (col %d) tag requires fixed size %d, schema has %d",
				f.Name, colIdx, spec.fixedSize, col.FixedSize)
		}
	}

	ft := f.Type
	switch col.Type {
	case TypeText, TypeTextFixed:
		if ft.Kind() != reflect.String {
			return fmt.Errorf("field %s (col %d, %s) must be string, got %s", f.Name, colIdx, col.Type, ft)
		}
		return nil

	case TypeBlob, TypeBlobFixed:
		if ft.Kind() == reflect.Slice && ft.Elem().Kind() == reflect.Uint8 {
			return nil
		}
		if ft.Kind() == reflect.Array && ft.Elem().Kind() == reflect.Uint8 {
			if col.Type == TypeBlobFixed && uint64(ft.Len()) != col.FixedSize {
				return fmt.Errorf("field %s (col %d, BlobFixed(%d)) array length %d != %d",
					f.Name, colIdx, col.FixedSize, ft.Len(), col.FixedSize)
			}
			return nil
		}
		return fmt.Errorf("field %s (col %d, %s) must be []byte or [N]byte, got %s", f.Name, colIdx, col.Type, ft)

	case TypeUShort:
		return checkIntFieldBits(f, colIdx, col, 16, false)

	case TypeUInt:
		return checkIntFieldBits(f, colIdx, col, 32, false)

	case TypeULong, TypeUNum, TypeUInt64:
		return checkIntFieldBits(f, colIdx, col, 64, true)

	case TypeUNumFixed:
		return checkIntFieldBits(f, colIdx, col, int(col.FixedSize)*8, true)

	default:
		return fmt.Errorf("unsupported schema type %s at column %d", col.Type, colIdx)
	}
}

// checkIntFieldBits verifies ft is an integer type wide enough to represent
// minBits without silent truncation. When allowExactSigned is true, a signed
// integer of bit width == minBits (e.g. int64 for 64-bit values) is accepted.
func checkIntFieldBits(f reflect.StructField, colIdx int, col ColumnSchema, minBits int, allowExactSigned bool) error {
	ft := f.Type
	switch ft.Kind() {
	case reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uint:
		if ft.Bits() < minBits {
			return fmt.Errorf("field %s (col %d, %s) unsigned type %s (%d bits) is narrower than required %d bits",
				f.Name, colIdx, col.Type, ft, ft.Bits(), minBits)
		}
		return nil

	case reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Int:
		required := minBits + 1
		if allowExactSigned {
			required = minBits
		}
		if ft.Bits() < required {
			return fmt.Errorf("field %s (col %d, %s) signed type %s (%d bits) is too narrow (need at least %d bits)",
				f.Name, colIdx, col.Type, ft, ft.Bits(), required)
		}
		return nil

	default:
		return fmt.Errorf("field %s (col %d, %s) must be an integer type, got %s", f.Name, colIdx, col.Type, ft)
	}
}

func applyRowToStruct(row Row, plan *structPlan, dstStruct reflect.Value) error {
	for _, b := range plan.bindings {
		val := row[b.colIndex]
		fv := dstStruct.FieldByIndex(b.fieldIndex)
		if err := assignValueToField(val, b.col, fv); err != nil {
			return fmt.Errorf("field %s (col %d): %w", b.fieldName, b.colIndex, err)
		}
	}
	return nil
}

func assignValueToField(val Value, col ColumnSchema, fv reflect.Value) error {
	ft := fv.Type()
	switch val.Kind() {
	case KindString:
		fv.SetString(val.strVal)
		return nil

	case KindBytes:
		if ft.Kind() == reflect.Slice {
			cp := make([]byte, len(val.bytesVal))
			copy(cp, val.bytesVal)
			fv.SetBytes(cp)
			return nil
		}
		if len(val.bytesVal) != ft.Len() {
			return fmt.Errorf("blob length %d does not fit [%d]byte", len(val.bytesVal), ft.Len())
		}
		reflect.Copy(fv, reflect.ValueOf(val.bytesVal))
		return nil

	case KindUint:
		switch fv.Kind() {
		case reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uint:
			if fv.OverflowUint(val.uintVal) {
				return fmt.Errorf("value %d overflows %s", val.uintVal, ft)
			}
			fv.SetUint(val.uintVal)
			return nil

		case reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Int:
			if (col.Type == TypeUNumFixed && ft.Bits() == int(col.FixedSize)*8) || (col.Type == TypeUInt64 && ft.Bits() == 64) {
				fv.SetInt(signExtendBits(val.uintVal, ft.Bits()))
				return nil
			}
			if val.uintVal > math.MaxInt64 || fv.OverflowInt(int64(val.uintVal)) {
				return fmt.Errorf("value %d overflows %s", val.uintVal, ft)
			}
			fv.SetInt(int64(val.uintVal))
			return nil
		}
	}
	return fmt.Errorf("cannot assign %s to %s", val.Type(), ft)
}

func signExtendBits(u uint64, bits int) int64 {
	if bits >= 64 {
		return int64(u)
	}
	shift := 64 - bits
	return int64(u<<shift) >> shift
}
