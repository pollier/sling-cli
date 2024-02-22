package iop

import (
	// "encoding/csv"
	// "io"

	"io"
	"math/big"
	"reflect"
	"runtime/debug"
	"strings"
	"time"

	arrowParquet "github.com/apache/arrow/go/v16/parquet"
	"github.com/flarco/g"
	"github.com/google/uuid"
	"github.com/samber/lo"
	"github.com/spf13/cast"

	parquet "github.com/parquet-go/parquet-go"
	"github.com/parquet-go/parquet-go/compress"
	"github.com/parquet-go/parquet-go/deprecated"
	"github.com/parquet-go/parquet-go/encoding"
	"github.com/parquet-go/parquet-go/format"
)

// Parquet is a parquet object
type Parquet struct {
	Path   string
	Reader *parquet.Reader
	Data   *Dataset
	colMap map[string]int
}

func NewParquetReader(reader io.ReaderAt, columns Columns) (p *Parquet, err error) {
	pr := parquet.NewReader(reader)
	if err != nil {
		err = g.Error(err, "could not get parquet reader")
		return
	}
	p = &Parquet{Reader: pr}
	p.colMap = p.Columns().FieldMap(true)
	return
}

func (p *Parquet) Columns() Columns {
	schema := p.Reader.Schema()

	typeMap := map[parquet.Type]ColumnType{
		parquet.BooleanType:   BoolType,
		parquet.Int32Type:     IntegerType,
		parquet.Int64Type:     BigIntType,
		parquet.Int96Type:     BigIntType,
		parquet.FloatType:     DecimalType,
		parquet.DoubleType:    DecimalType,
		parquet.ByteArrayType: StringType,
	}

	cols := Columns{}
	for _, field := range schema.Fields() {
		colType := field.Type()
		if colType == nil {
			colType = parquet.ByteArrayType
		}

		c := Column{
			Name:     CleanName(field.Name()),
			Type:     typeMap[colType],
			Position: len(cols) + 1,
			Sourced:  !g.In(typeMap[colType], DecimalType),
		}

		cols = append(cols, c)
	}
	return cols
}

func (p *Parquet) nextFunc(it *Iterator) bool {
	// recover from panic
	defer func() {
		if r := recover(); r != nil {
			g.Warn("recovered from panic: %#v\n%s", r, string(debug.Stack()))
			err := g.Error("panic occurred! %#v", r)
			it.Context.CaptureErr(err)
		}
	}()

	row := map[string]any{}
	err := p.Reader.Read(&row)
	if err == io.EOF {
		return false
	} else if err != nil {
		it.Context.CaptureErr(g.Error(err, "could not read Parquet row"))
		return false
	}

	it.Row = make([]interface{}, len(it.ds.Columns))
	for k, v := range row {
		col := it.ds.Columns[p.colMap[strings.ToLower(k)]]
		i := col.Position - 1
		it.Row[i] = v
	}
	return true
}

type ParquetWriter struct {
	Writer      *parquet.Writer
	columns     Columns
	decNumScale []*big.Rat
}

func NewParquetWriter(w io.Writer, columns Columns, codec compress.Codec) (p *ParquetWriter, err error) {

	// make scale big.Rat numbers
	decNumScale := make([]*big.Rat, len(columns))
	for i, col := range columns {
		col.DbPrecision = lo.Ternary(col.DbPrecision == 0, 28, lo.Ternary(col.DbPrecision > 36, 36, col.DbPrecision))
		col.DbScale = lo.Ternary(col.DbScale == 0, 9, lo.Ternary(col.DbScale > 16, 16, col.DbScale))
		columns[i] = col

		if col.DbScale > 0 {
			decNumScale[i] = MakeDecNumScale(col.DbScale)
		} else {
			decNumScale[i] = MakeDecNumScale(1)
		}
	}

	config, err := parquet.NewWriterConfig()
	if err != nil {
		return nil, g.Error(err, "could not create parquet writer config")
	}
	config.Schema = getParquetSchema(columns)
	config.Compression = codec

	fw := parquet.NewWriter(w, config)

	return &ParquetWriter{
		Writer:      fw,
		columns:     columns,
		decNumScale: decNumScale,
	}, nil

}

func (pw *ParquetWriter) WriteRow(row []any) error {
	rec := make([]parquet.Value, len(pw.columns))

	for i, col := range pw.columns {
		switch {
		case col.IsBool():
			row[i] = cast.ToBool(row[i]) // since is stored as string
		case col.Type == FloatType:
			row[i] = cast.ToFloat64(row[i])
		case col.Type == DecimalType:
			// row[i] = cast.ToString(row[i])
			row[i] = StringToDecimalByteArray(cast.ToString(row[i]), pw.decNumScale[i], arrowParquet.Types.FixedLenByteArray, 16)
		case col.IsDatetime():
			switch valT := row[i].(type) {
			case time.Time:
				if row[i] != nil {
					switch col.DbPrecision {
					case 3:
						row[i] = valT.UnixMilli()
					case 6:
						row[i] = valT.UnixMicro()
					case 9:
						row[i] = valT.UnixNano()
					default:
						row[i] = valT.UnixNano()
					}
				}
			}
		}
		if i < len(row) {
			rec[i] = parquet.ValueOf(row[i])
		}
	}

	_, err := pw.Writer.WriteRows([]parquet.Row{rec})
	if err != nil {
		return g.Error(err, "error writing row")
	}

	return nil
}

func (pw *ParquetWriter) Close() error {
	return pw.Writer.Close()
}

func getParquetSchema(cols Columns) *parquet.Schema {
	return parquet.NewSchema("", NewRecNode(cols))
}

func NewRecNode(cols Columns) *RecNode {

	rn := &RecNode{
		fields: make([]structField, len(cols)),
	}

	for i, col := range cols {
		field := structField{name: col.Name, index: []int{col.Position - 1}}
		field.Node = nodeOf(col, []string{})
		rn.fields[i] = field
	}

	return rn
}

type RecNode struct {
	fields []structField
}

func (rn *RecNode) ID() int { return 0 }

func (rn *RecNode) String() string { return "" }

func (rn *RecNode) Type() parquet.Type { return groupType{} }

func (rn *RecNode) Optional() bool { return false }

func (rn *RecNode) Repeated() bool { return false }

func (rn *RecNode) Required() bool { return true }

func (rn *RecNode) Leaf() bool { return false }

func (rn *RecNode) Fields() []parquet.Field {
	fields := make([]parquet.Field, len(rn.fields))
	for i := range rn.fields {
		fields[i] = &rn.fields[i]
	}
	return fields
}

func (rn *RecNode) Encoding() encoding.Encoding { return nil }

func (rn *RecNode) Compression() compress.Codec { return nil }
func (rn *RecNode) GoType() reflect.Type        { return nil }

type structField struct {
	parquet.Node
	name  string
	index []int
}

func (f *structField) Name() string { return f.name }

func (f *structField) Value(base reflect.Value) reflect.Value {
	switch base.Kind() {
	case reflect.Map:
		return base.MapIndex(reflect.ValueOf(&f.name).Elem())
	case reflect.Ptr:
		if base.IsNil() {
			base.Set(reflect.New(base.Type().Elem()))
		}
		return fieldByIndex(base.Elem(), f.index)
	default:
		if len(f.index) == 1 {
			return base.Field(f.index[0])
		} else {
			return fieldByIndex(base, f.index)
		}
	}
}

// fieldByIndex is like reflect.Value.FieldByIndex but returns the zero-value of
// reflect.Value if one of the fields was a nil pointer instead of panicking.
func fieldByIndex(v reflect.Value, index []int) reflect.Value {
	for _, i := range index {
		if v = v.Field(i); v.Kind() == reflect.Ptr || v.Kind() == reflect.Interface {
			if v.IsNil() {
				v.Set(reflect.New(v.Type().Elem()))
				v = v.Elem()
				break
			} else {
				v = v.Elem()
			}
		}
	}
	return v
}

type groupType struct{}

func (groupType) String() string { return "group" }

func (groupType) Kind() parquet.Kind {
	panic("cannot call Kind on parquet group")
}

func (groupType) Compare(parquet.Value, parquet.Value) int {
	panic("cannot compare values on parquet group")
}

func (groupType) NewColumnIndexer(int) parquet.ColumnIndexer {
	panic("cannot create column indexer from parquet group")
}

func (groupType) NewDictionary(int, int, encoding.Values) parquet.Dictionary {
	panic("cannot create dictionary from parquet group")
}

func (t groupType) NewColumnBuffer(int, int) parquet.ColumnBuffer {
	panic("cannot create column buffer from parquet group")
}

func (t groupType) NewPage(int, int, encoding.Values) parquet.Page {
	panic("cannot create page from parquet group")
}

func (t groupType) NewValues(_ []byte, _ []uint32) encoding.Values {
	panic("cannot create values from parquet group")
}

func (groupType) Encode(_ []byte, _ encoding.Values, _ encoding.Encoding) ([]byte, error) {
	panic("cannot encode parquet group")
}

func (groupType) Decode(_ encoding.Values, _ []byte, _ encoding.Encoding) (encoding.Values, error) {
	panic("cannot decode parquet group")
}

func (groupType) EstimateDecodeSize(_ int, _ []byte, _ encoding.Encoding) int {
	panic("cannot estimate decode size of parquet group")
}

func (groupType) AssignValue(reflect.Value, parquet.Value) error {
	panic("cannot assign value to a parquet group")
}

func (t groupType) ConvertValue(parquet.Value, parquet.Type) (parquet.Value, error) {
	panic("cannot convert value to a parquet group")
}

func (groupType) Length() int { return 0 }

func (groupType) EstimateSize(int) int { return 0 }

func (groupType) EstimateNumValues(int) int { return 0 }

func (groupType) ColumnOrder() *format.ColumnOrder { return nil }

func (groupType) PhysicalType() *format.Type { return nil }

func (groupType) LogicalType() *format.LogicalType { return nil }

func (groupType) ConvertedType() *deprecated.ConvertedType { return nil }

func nodeOf(col Column, tag []string) parquet.Node {

	switch col.GoType() {
	case reflect.TypeOf(deprecated.Int96{}):
		return parquet.Leaf(parquet.Int96Type)
	case reflect.TypeOf(uuid.UUID{}):
		return parquet.UUID()
	case reflect.TypeOf(time.Time{}):
		newType := parquet.Timestamp(parquet.Nanosecond)
		switch col.DbPrecision {
		case 3:
			newType = parquet.Timestamp(parquet.Millisecond)
		case 6:
			newType = parquet.Timestamp(parquet.Microsecond)
		case 9:
			newType = parquet.Timestamp(parquet.Nanosecond)
		}
		return newType
	}

	var n parquet.Node
	switch col.Type {
	case FloatType:
		n = parquet.Leaf(parquet.DoubleType)
		return &goNode{Node: n, gotype: col.GoType()}
	case DecimalType:
		n = parquet.Decimal(col.DbScale, col.DbPrecision, parquet.FixedLenByteArrayType(16))
		return &goNode{Node: n, gotype: col.GoType()}
	}

	switch col.GoType().Kind() {
	case reflect.Bool:
		n = parquet.Leaf(parquet.BooleanType)

	case reflect.Int, reflect.Int64:
		n = parquet.Int(64)

	case reflect.Int8, reflect.Int16, reflect.Int32:
		n = parquet.Int(col.GoType().Bits())

	case reflect.Uint, reflect.Uintptr, reflect.Uint64:
		n = parquet.Uint(64)

	case reflect.Uint8, reflect.Uint16, reflect.Uint32:
		n = parquet.Uint(col.GoType().Bits())

	case reflect.Float32:
		n = parquet.Leaf(parquet.FloatType)

	case reflect.Float64:
		n = parquet.Leaf(parquet.DoubleType)

	case reflect.String:
		n = parquet.String()

	case reflect.Ptr:
		col.goType = col.GoType().Elem()
		n = parquet.Optional(nodeOf(col, nil))

	case reflect.Slice:
		if elem := col.GoType().Elem(); elem.Kind() == reflect.Uint8 { // []byte?
			n = parquet.Leaf(parquet.ByteArrayType)
		} else {
			col.goType = elem
			n = parquet.Repeated(nodeOf(col, nil))
		}

	case reflect.Array:
		if col.GoType().Elem().Kind() == reflect.Uint8 {
			n = parquet.Leaf(parquet.FixedLenByteArrayType(col.GoType().Len()))
		}

	case reflect.Map:
		n = parquet.JSON()

	}

	return &goNode{Node: n, gotype: col.GoType()}
}

type goNode struct {
	parquet.Node
	gotype reflect.Type
}
