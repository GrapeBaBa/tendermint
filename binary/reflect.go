package binary

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"sync"
	"time"

	. "github.com/tendermint/tendermint/common"
)

type TypeInfo struct {
	Type reflect.Type // The type

	// If Type is kind reflect.Interface, is registered
	IsRegisteredInterface bool
	ConcreteTypes         map[byte]reflect.Type
	ConcreteTypeBytes     map[reflect.Type]byte

	// If Type is concrete
	HasTypeByte bool
	TypeByte    byte

	// If Type is kind reflect.Struct
	Fields []StructFieldInfo
}

type StructFieldInfo struct {
	Index    int          // Struct field index
	JSONName string       // Corresponding JSON field name. (override with `json=""`)
	Type     reflect.Type // Struct field type
}

func (info StructFieldInfo) unpack() (int, string, reflect.Type) {
	return info.Index, info.JSONName, info.Type
}

// e.g. If o is struct{Foo}{}, return is the Foo interface type.
func GetTypeFromStructDeclaration(o interface{}) reflect.Type {
	rt := reflect.TypeOf(o)
	if rt.NumField() != 1 {
		panic("Unexpected number of fields in struct-wrapped declaration of type")
	}
	return rt.Field(0).Type
}

// If o implements HasTypeByte, returns (true, typeByte)
func GetTypeByteFromStruct(o interface{}) (hasTypeByte bool, typeByte byte) {
	if _, ok := o.(HasTypeByte); ok {
		return true, o.(HasTypeByte).TypeByte()
	} else {
		return false, byte(0x00)
	}
}

// Predeclaration of common types
var (
	timeType = GetTypeFromStructDeclaration(struct{ time.Time }{})
)

const (
	rfc2822 = "Mon Jan 02 15:04:05 -0700 2006"
)

// If a type implements TypeByte, the byte is included
// as the first byte for encoding and decoding.
// This is primarily used to encode interfaces types.
// The interface should be declared with RegisterInterface()
type HasTypeByte interface {
	TypeByte() byte
}

// NOTE: do not access typeInfos directly, but call GetTypeInfo()
var typeInfosMtx sync.Mutex
var typeInfos = map[reflect.Type]*TypeInfo{}

func GetTypeInfo(rt reflect.Type) *TypeInfo {
	typeInfosMtx.Lock()
	defer typeInfosMtx.Unlock()
	info := typeInfos[rt]
	if info == nil {
		info = &TypeInfo{Type: rt}
		RegisterType(info)
	}
	return info
}

// For use with the RegisterInterface declaration
type ConcreteType struct {
	O interface{}
}

// Must use this to register an interface to properly decode the
// underlying concrete type.
func RegisterInterface(o interface{}, args ...interface{}) *TypeInfo {
	it := GetTypeFromStructDeclaration(o)
	if it.Kind() != reflect.Interface {
		panic("RegisterInterface expects an interface")
	}
	concreteTypes := make(map[byte]reflect.Type, 0)
	concreteTypesReversed := make(map[reflect.Type]byte, 0)
	for _, arg := range args {
		switch arg.(type) {
		case ConcreteType:
			concreteTypeInfo := arg.(ConcreteType)
			concreteType := reflect.TypeOf(concreteTypeInfo.O)
			hasTypeByte, typeByte := GetTypeByteFromStruct(concreteTypeInfo.O)
			//fmt.Println(Fmt("HasTypeByte: %v typeByte: %X type: %X", hasTypeByte, typeByte, concreteType))
			if !hasTypeByte {
				panic(Fmt("Expected concrete type %v to implement HasTypeByte", concreteType))
			}
			if typeByte == 0x00 {
				panic(Fmt("TypeByte of 0x00 is reserved for nil (%v)", concreteType))
			}
			if concreteTypes[typeByte] != nil {
				panic(Fmt("Duplicate TypeByte for type %v and %v", concreteType, concreteTypes[typeByte]))
			}
			concreteTypes[typeByte] = concreteType
			concreteTypesReversed[concreteType] = typeByte
		default:
			panic(Fmt("Unexpected argument type %v", reflect.TypeOf(arg)))
		}
	}
	typeInfo := &TypeInfo{
		Type: it,
		IsRegisteredInterface: true,
		ConcreteTypes:         concreteTypes,
		ConcreteTypeBytes:     concreteTypesReversed,
	}
	typeInfos[it] = typeInfo
	return typeInfo
}

// Registers and possibly modifies the TypeInfo.
// NOTE: not goroutine safe, so only call upon program init.
func RegisterType(info *TypeInfo) *TypeInfo {

	// Also register the dereferenced struct if info.Type is a pointer.
	// Or, if info.Type is not a pointer, register the pointer.
	var rt, ptrRt reflect.Type
	if info.Type.Kind() == reflect.Ptr {
		rt, ptrRt = info.Type.Elem(), info.Type
	} else {
		rt, ptrRt = info.Type, reflect.PtrTo(info.Type)
	}

	// Register the type info
	typeInfos[rt] = info
	typeInfos[ptrRt] = info

	// See if the type implements HasTypeByte
	if rt.Kind() != reflect.Interface &&
		rt.Implements(reflect.TypeOf((*HasTypeByte)(nil)).Elem()) {
		zero := reflect.Zero(rt)
		typeByte := zero.Interface().(HasTypeByte).TypeByte()
		if info.HasTypeByte && info.TypeByte != typeByte {
			panic(Fmt("Type %v expected TypeByte of %X", rt, typeByte))
		} else {
			info.HasTypeByte = true
			info.TypeByte = typeByte
		}
	} else if ptrRt.Implements(reflect.TypeOf((*HasTypeByte)(nil)).Elem()) {
		zero := reflect.Zero(ptrRt)
		typeByte := zero.Interface().(HasTypeByte).TypeByte()
		if info.HasTypeByte && info.TypeByte != typeByte {
			panic(Fmt("Type %v expected TypeByte of %X", ptrRt, typeByte))
		} else {
			info.HasTypeByte = true
			info.TypeByte = typeByte
		}
	}

	// If struct, register field name options
	if rt.Kind() == reflect.Struct {
		numFields := rt.NumField()
		structFields := []StructFieldInfo{}
		for i := 0; i < numFields; i++ {
			field := rt.Field(i)
			if field.PkgPath != "" {
				continue
			}
			jsonName := field.Tag.Get("json")
			if jsonName == "-" {
				continue
			} else if jsonName == "" {
				jsonName = field.Name
			}
			structFields = append(structFields, StructFieldInfo{
				Index:    i,
				JSONName: jsonName,
				Type:     field.Type,
			})
		}
		info.Fields = structFields
	}

	return info
}

func readReflect(rv reflect.Value, rt reflect.Type, r io.Reader, n *int64, err *error) {

	// Get typeInfo
	typeInfo := GetTypeInfo(rt)

	if rt.Kind() == reflect.Interface {
		if !typeInfo.IsRegisteredInterface {
			// There's no way we can read such a thing.
			*err = errors.New(Fmt("Cannot read unregistered interface type %v", rt))
			return
		}
		typeByte := ReadByte(r, n, err)
		if *err != nil {
			return
		}
		if typeByte == 0x00 {
			return // nil
		}
		crt, ok := typeInfo.ConcreteTypes[typeByte]
		if !ok {
			*err = errors.New(Fmt("Unexpected type byte %X for type %v", typeByte, crt))
			return
		}
		crv := reflect.New(crt).Elem()
		r = NewPrefixedReader([]byte{typeByte}, r)
		readReflect(crv, crt, r, n, err)
		rv.Set(crv) // NOTE: orig rv is ignored.
		return
	}

	if rt.Kind() == reflect.Ptr {
		typeByte := ReadByte(r, n, err)
		if *err != nil {
			return
		}
		if typeByte == 0x00 {
			return // nil
		}
		// Create new if rv is nil.
		if rv.IsNil() {
			newRv := reflect.New(rt.Elem())
			rv.Set(newRv)
			rv = newRv
		}
		// Dereference pointer
		rv, rt = rv.Elem(), rt.Elem()
		typeInfo = GetTypeInfo(rt)
		if typeInfo.HasTypeByte {
			r = NewPrefixedReader([]byte{typeByte}, r)
		}
		// continue...
	}

	// Read TypeByte prefix
	if typeInfo.HasTypeByte {
		typeByte := ReadByte(r, n, err)
		if typeByte != typeInfo.TypeByte {
			*err = errors.New(Fmt("Expected TypeByte of %X but got %X", typeInfo.TypeByte, typeByte))
			return
		}
	}

	switch rt.Kind() {
	case reflect.Slice:
		elemRt := rt.Elem()
		if elemRt.Kind() == reflect.Uint8 {
			// Special case: Byteslices
			byteslice := ReadByteSlice(r, n, err)
			log.Debug("Read byteslice", "bytes", byteslice)
			rv.Set(reflect.ValueOf(byteslice))
		} else {
			// Read length
			length := int(ReadUvarint(r, n, err))
			log.Debug(Fmt("Read length: %v", length))
			sliceRv := reflect.MakeSlice(rt, length, length)
			// Read elems
			for i := 0; i < length; i++ {
				elemRv := sliceRv.Index(i)
				readReflect(elemRv, elemRt, r, n, err)
			}
			rv.Set(sliceRv)
		}

	case reflect.Struct:
		if rt == timeType {
			// Special case: time.Time
			t := ReadTime(r, n, err)
			log.Debug(Fmt("Read time: %v", t))
			rv.Set(reflect.ValueOf(t))
		} else {
			numFields := rt.NumField()
			for i := 0; i < numFields; i++ {
				field := rt.Field(i)
				if field.PkgPath != "" {
					continue
				}
				fieldRv := rv.Field(i)
				readReflect(fieldRv, field.Type, r, n, err)
			}
		}

	case reflect.String:
		str := ReadString(r, n, err)
		log.Debug(Fmt("Read string: %v", str))
		rv.SetString(str)

	case reflect.Int64:
		num := ReadUint64(r, n, err)
		log.Debug(Fmt("Read num: %v", num))
		rv.SetInt(int64(num))

	case reflect.Int32:
		num := ReadUint32(r, n, err)
		log.Debug(Fmt("Read num: %v", num))
		rv.SetInt(int64(num))

	case reflect.Int16:
		num := ReadUint16(r, n, err)
		log.Debug(Fmt("Read num: %v", num))
		rv.SetInt(int64(num))

	case reflect.Int8:
		num := ReadUint8(r, n, err)
		log.Debug(Fmt("Read num: %v", num))
		rv.SetInt(int64(num))

	case reflect.Int:
		num := ReadUvarint(r, n, err)
		log.Debug(Fmt("Read num: %v", num))
		rv.SetInt(int64(num))

	case reflect.Uint64:
		num := ReadUint64(r, n, err)
		log.Debug(Fmt("Read num: %v", num))
		rv.SetUint(uint64(num))

	case reflect.Uint32:
		num := ReadUint32(r, n, err)
		log.Debug(Fmt("Read num: %v", num))
		rv.SetUint(uint64(num))

	case reflect.Uint16:
		num := ReadUint16(r, n, err)
		log.Debug(Fmt("Read num: %v", num))
		rv.SetUint(uint64(num))

	case reflect.Uint8:
		num := ReadUint8(r, n, err)
		log.Debug(Fmt("Read num: %v", num))
		rv.SetUint(uint64(num))

	case reflect.Uint:
		num := ReadUvarint(r, n, err)
		log.Debug(Fmt("Read num: %v", num))
		rv.SetUint(uint64(num))

	case reflect.Bool:
		num := ReadUint8(r, n, err)
		log.Debug(Fmt("Read bool: %v", num))
		rv.SetBool(num > 0)

	default:
		panic(Fmt("Unknown field type %v", rt.Kind()))
	}
}

// rv: the reflection value of the thing to write
// rt: the type of rv as declared in the container, not necessarily rv.Type().
func writeReflect(rv reflect.Value, rt reflect.Type, w io.Writer, n *int64, err *error) {

	// Get typeInfo
	typeInfo := GetTypeInfo(rt)

	if rt.Kind() == reflect.Interface {
		if rv.IsNil() {
			// XXX ensure that typeByte 0 is reserved.
			WriteByte(0x00, w, n, err)
			return
		}
		crv := rv.Elem()  // concrete reflection value
		crt := crv.Type() // concrete reflection type
		if typeInfo.IsRegisteredInterface {
			// See if the crt is registered.
			// If so, we're more restrictive.
			_, ok := typeInfo.ConcreteTypeBytes[crt]
			if !ok {
				switch crt.Kind() {
				case reflect.Ptr:
					*err = errors.New(Fmt("Unexpected pointer type %v. Was it registered as a value receiver rather than as a pointer receiver?", crt))
				case reflect.Struct:
					*err = errors.New(Fmt("Unexpected struct type %v. Was it registered as a pointer receiver rather than as a value receiver?", crt))
				default:
					*err = errors.New(Fmt("Unexpected type %v.", crt))
				}
				return
			}
		} else {
			// We support writing unsafely for convenience.
		}
		// We don't have to write the typeByte here,
		// the writeReflect() call below will write it.
		writeReflect(crv, crt, w, n, err)
		return
	}

	if rt.Kind() == reflect.Ptr {
		// Dereference pointer
		rv, rt = rv.Elem(), rt.Elem()
		if !rv.IsValid() {
			WriteByte(0x00, w, n, err)
			return
		}
		if !typeInfo.HasTypeByte {
			WriteByte(0x01, w, n, err)
			// continue...
		} else {
			// continue...
		}
	}

	// Write type byte
	if typeInfo.HasTypeByte {
		WriteByte(typeInfo.TypeByte, w, n, err)
	}

	// All other types
	switch rt.Kind() {
	case reflect.Slice:
		elemRt := rt.Elem()
		if elemRt.Kind() == reflect.Uint8 {
			// Special case: Byteslices
			byteslice := rv.Bytes()
			WriteByteSlice(byteslice, w, n, err)
		} else {
			// Write length
			length := rv.Len()
			WriteUvarint(uint(length), w, n, err)
			// Write elems
			for i := 0; i < length; i++ {
				elemRv := rv.Index(i)
				writeReflect(elemRv, elemRt, w, n, err)
			}
		}

	case reflect.Struct:
		if rt == timeType {
			// Special case: time.Time
			WriteTime(rv.Interface().(time.Time), w, n, err)
		} else {
			numFields := rt.NumField()
			for i := 0; i < numFields; i++ {
				field := rt.Field(i)
				if field.PkgPath != "" {
					continue
				}
				fieldRv := rv.Field(i)
				writeReflect(fieldRv, field.Type, w, n, err)
			}
		}

	case reflect.String:
		WriteString(rv.String(), w, n, err)

	case reflect.Int64:
		WriteInt64(rv.Int(), w, n, err)

	case reflect.Int32:
		WriteInt32(int32(rv.Int()), w, n, err)

	case reflect.Int16:
		WriteInt16(int16(rv.Int()), w, n, err)

	case reflect.Int8:
		WriteInt8(int8(rv.Int()), w, n, err)

	case reflect.Int:
		WriteVarint(int(rv.Int()), w, n, err)

	case reflect.Uint64:
		WriteUint64(rv.Uint(), w, n, err)

	case reflect.Uint32:
		WriteUint32(uint32(rv.Uint()), w, n, err)

	case reflect.Uint16:
		WriteUint16(uint16(rv.Uint()), w, n, err)

	case reflect.Uint8:
		WriteUint8(uint8(rv.Uint()), w, n, err)

	case reflect.Uint:
		WriteUvarint(uint(rv.Uint()), w, n, err)

	case reflect.Bool:
		if rv.Bool() {
			WriteUint8(uint8(1), w, n, err)
		} else {
			WriteUint8(uint8(0), w, n, err)
		}

	default:
		panic(Fmt("Unknown field type %v", rt.Kind()))
	}
}

//-----------------------------------------------------------------------------

func readTypeByteJSON(o interface{}) (typeByte byte, rest interface{}, err error) {
	oSlice, ok := o.([]interface{})
	if !ok {
		err = errors.New(Fmt("Expected type [TypeByte,?] but got type %v", reflect.TypeOf(o)))
		return
	}
	if len(oSlice) != 2 {
		err = errors.New(Fmt("Expected [TypeByte,?] len 2 but got len %v", len(oSlice)))
		return
	}
	typeByte_, ok := oSlice[0].(float64)
	typeByte = byte(typeByte_)
	rest = oSlice[1]
	return
}

func readReflectJSON(rv reflect.Value, rt reflect.Type, o interface{}, err *error) {

	// Get typeInfo
	typeInfo := GetTypeInfo(rt)

	if rt.Kind() == reflect.Interface {
		if !typeInfo.IsRegisteredInterface {
			// There's no way we can read such a thing.
			*err = errors.New(Fmt("Cannot read unregistered interface type %v", rt))
			return
		}
		if o == nil {
			return // nil
		}
		typeByte, _, err_ := readTypeByteJSON(o)
		if err_ != nil {
			*err = err_
			return
		}
		crt, ok := typeInfo.ConcreteTypes[typeByte]
		if !ok {
			*err = errors.New(Fmt("TypeByte %X not registered for interface %v", typeByte, rt))
			return
		}
		crv := reflect.New(crt).Elem()
		readReflectJSON(crv, crt, o, err)
		rv.Set(crv) // NOTE: orig rv is ignored.
		return
	}

	if rt.Kind() == reflect.Ptr {
		if o == nil {
			return // nil
		}
		// Create new struct if rv is nil.
		if rv.IsNil() {
			newRv := reflect.New(rt.Elem())
			rv.Set(newRv)
			rv = newRv
		}
		// Dereference pointer
		rv, rt = rv.Elem(), rt.Elem()
		// continue...
	}

	// Read TypeByte prefix
	if typeInfo.HasTypeByte {
		typeByte, rest, err_ := readTypeByteJSON(o)
		if err_ != nil {
			*err = err_
			return
		}
		if typeByte != typeInfo.TypeByte {
			*err = errors.New(Fmt("Expected TypeByte of %X but got %X", typeInfo.TypeByte, byte(typeByte)))
			return
		}
		o = rest
	}

	switch rt.Kind() {
	case reflect.Slice:
		elemRt := rt.Elem()
		if elemRt.Kind() == reflect.Uint8 {
			// Special case: Byteslices
			oString, ok := o.(string)
			if !ok {
				*err = errors.New(Fmt("Expected string but got type %v", reflect.TypeOf(o)))
				return
			}
			byteslice, err_ := hex.DecodeString(oString)
			if err_ != nil {
				*err = err_
				return
			}
			log.Debug("Read byteslice", "bytes", byteslice)
			rv.Set(reflect.ValueOf(byteslice))
		} else {
			// Read length
			oSlice, ok := o.([]interface{})
			if !ok {
				*err = errors.New(Fmt("Expected array of %v but got type %v", rt, reflect.TypeOf(o)))
				return
			}
			length := len(oSlice)
			log.Debug(Fmt("Read length: %v", length))
			sliceRv := reflect.MakeSlice(rt, length, length)
			// Read elems
			for i := 0; i < length; i++ {
				elemRv := sliceRv.Index(i)
				readReflectJSON(elemRv, elemRt, oSlice[i], err)
			}
			rv.Set(sliceRv)
		}

	case reflect.Struct:
		if rt == timeType {
			// Special case: time.Time
			str, ok := o.(string)
			if !ok {
				*err = errors.New(Fmt("Expected string but got type %v", reflect.TypeOf(o)))
				return
			}
			log.Debug(Fmt("Read time: %v", str))
			t, err_ := time.Parse(rfc2822, str)
			if err_ != nil {
				*err = err_
				return
			}
			rv.Set(reflect.ValueOf(t))
		} else {
			oMap, ok := o.(map[string]interface{})
			if !ok {
				*err = errors.New(Fmt("Expected map but got type %v", reflect.TypeOf(o)))
				return
			}
			// TODO: ensure that all fields are set?
			// TODO: disallow unknown oMap fields?
			for _, fieldInfo := range typeInfo.Fields {
				i, jsonName, fieldType := fieldInfo.unpack()
				value, ok := oMap[jsonName]
				if !ok {
					continue // Skip missing fields.
				}
				fieldRv := rv.Field(i)
				readReflectJSON(fieldRv, fieldType, value, err)
			}
		}

	case reflect.String:
		str, ok := o.(string)
		if !ok {
			*err = errors.New(Fmt("Expected string but got type %v", reflect.TypeOf(o)))
			return
		}
		log.Debug(Fmt("Read string: %v", str))
		rv.SetString(str)

	case reflect.Int64, reflect.Int32, reflect.Int16, reflect.Int8, reflect.Int:
		num, ok := o.(float64)
		if !ok {
			*err = errors.New(Fmt("Expected numeric but got type %v", reflect.TypeOf(o)))
			return
		}
		log.Debug(Fmt("Read num: %v", num))
		rv.SetInt(int64(num))

	case reflect.Uint64, reflect.Uint32, reflect.Uint16, reflect.Uint8, reflect.Uint:
		num, ok := o.(float64)
		if !ok {
			*err = errors.New(Fmt("Expected numeric but got type %v", reflect.TypeOf(o)))
			return
		}
		if num < 0 {
			*err = errors.New(Fmt("Expected unsigned numeric but got %v", num))
			return
		}
		log.Debug(Fmt("Read num: %v", num))
		rv.SetUint(uint64(num))

	case reflect.Bool:
		bl, ok := o.(bool)
		if !ok {
			*err = errors.New(Fmt("Expected boolean but got type %v", reflect.TypeOf(o)))
			return
		}
		log.Debug(Fmt("Read boolean: %v", bl))
		rv.SetBool(bl)

	default:
		panic(Fmt("Unknown field type %v", rt.Kind()))
	}
}

func writeReflectJSON(rv reflect.Value, rt reflect.Type, w io.Writer, n *int64, err *error) {

	// Get typeInfo
	typeInfo := GetTypeInfo(rt)

	if rt.Kind() == reflect.Interface {
		if rv.IsNil() {
			// XXX ensure that typeByte 0 is reserved.
			WriteTo([]byte("null"), w, n, err)
			return
		}
		crv := rv.Elem()  // concrete reflection value
		crt := crv.Type() // concrete reflection type
		if typeInfo.IsRegisteredInterface {
			// See if the crt is registered.
			// If so, we're more restrictive.
			_, ok := typeInfo.ConcreteTypeBytes[crt]
			if !ok {
				switch crt.Kind() {
				case reflect.Ptr:
					*err = errors.New(Fmt("Unexpected pointer type %v. Was it registered as a value receiver rather than as a pointer receiver?", crt))
				case reflect.Struct:
					*err = errors.New(Fmt("Unexpected struct type %v. Was it registered as a pointer receiver rather than as a value receiver?", crt))
				default:
					*err = errors.New(Fmt("Unexpected type %v.", crt))
				}
				return
			}
		} else {
			// We support writing unsafely for convenience.
		}
		// We don't have to write the typeByte here,
		// the writeReflectJSON() call below will write it.
		writeReflectJSON(crv, crt, w, n, err)
		return
	}

	if rt.Kind() == reflect.Ptr {
		// Dereference pointer
		rv, rt = rv.Elem(), rt.Elem()
		if !rv.IsValid() {
			WriteTo([]byte("null"), w, n, err)
			return
		}
		// continue...
	}

	// Write TypeByte
	if typeInfo.HasTypeByte {
		WriteTo([]byte(Fmt("[%v,", typeInfo.TypeByte)), w, n, err)
		defer WriteTo([]byte("]"), w, n, err)
	}

	// All other types
	switch rt.Kind() {
	case reflect.Slice:
		elemRt := rt.Elem()
		if elemRt.Kind() == reflect.Uint8 {
			// Special case: Byteslices
			byteslice := rv.Bytes()
			WriteTo([]byte(Fmt("\"%X\"", byteslice)), w, n, err)
			//WriteByteSlice(byteslice, w, n, err)
		} else {
			WriteTo([]byte("["), w, n, err)
			// Write elems
			length := rv.Len()
			for i := 0; i < length; i++ {
				elemRv := rv.Index(i)
				writeReflectJSON(elemRv, elemRt, w, n, err)
				if i < length-1 {
					WriteTo([]byte(","), w, n, err)
				}
			}
			WriteTo([]byte("]"), w, n, err)
		}

	case reflect.Struct:
		if rt == timeType {
			// Special case: time.Time
			t := rv.Interface().(time.Time)
			str := t.Format(rfc2822)
			jsonBytes, err_ := json.Marshal(str)
			if err_ != nil {
				*err = err_
				return
			}
			WriteTo(jsonBytes, w, n, err)
		} else {
			WriteTo([]byte("{"), w, n, err)
			wroteField := false
			for _, fieldInfo := range typeInfo.Fields {
				i, jsonName, fieldType := fieldInfo.unpack()
				fieldRv := rv.Field(i)
				if wroteField {
					WriteTo([]byte(","), w, n, err)
				} else {
					wroteField = true
				}
				WriteTo([]byte(Fmt("\"%v\":", jsonName)), w, n, err)
				writeReflectJSON(fieldRv, fieldType, w, n, err)
			}
			WriteTo([]byte("}"), w, n, err)
		}

	case reflect.String:
		fallthrough
	case reflect.Uint64, reflect.Uint32, reflect.Uint16, reflect.Uint8, reflect.Uint:
		fallthrough
	case reflect.Int64, reflect.Int32, reflect.Int16, reflect.Int8, reflect.Int:
		fallthrough
	case reflect.Bool:
		jsonBytes, err_ := json.Marshal(rv.Interface())
		if err_ != nil {
			*err = err_
			return
		}
		WriteTo(jsonBytes, w, n, err)

	default:
		panic(Fmt("Unknown field type %v", rt.Kind()))
	}

}
