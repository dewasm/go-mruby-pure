package mruby

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
)

// Decode fills out, a non-nil pointer, from the Ruby value v.
//
// The conversions accepted, by the kind of out's pointee:
//
//   - bool: Ruby true or false only.
//   - every signed and unsigned integer kind: a Ruby Integer that fits the target's width; a uint rejects a negative Integer.
//   - float32 and float64: a Ruby Float or Integer, widened the way [Value.AsFloat] does; float32 also checks the value fits.
//   - string: a Ruby String or a Symbol, which decodes to its name.
//   - [Symbol]: a Ruby Symbol only.
//   - [Value]: any Ruby value; the field just receives v itself, so the caller then owns its lifetime.
//   - a slice: a Ruby Array, decoded element by element; []byte instead takes a Ruby String.
//   - a map: a Ruby Hash, whose key type must be string, [Symbol] or any; a String key and a Symbol key both fill a string-keyed map, the Symbol as its name.
//   - a pointer: allocated when nil and decoded into; Ruby nil sets the pointer to nil instead.
//   - any: the shapes [Value.GoValue] produces.
//   - a struct: a Ruby Hash, matched to fields by key (a String key and a Symbol key both match), or any other Ruby value, whose zero-argument methods are called by field name; a missing method leaves the field untouched.
//
// A struct field is named by its `mruby` tag, or by strings.ToLower of the Go field name when there is none; `mruby:"-"` skips the field.
// The tag option `,squash` on an anonymous embedded struct field decodes it from the same source value as the struct around it, rather than from a field of its own.
// Unexported fields are skipped.
//
// Ruby nil into anything but a pointer or any is an error, except that it leaves a [Value] field as a nil *Value.
// An error names the field it was found in, in the style "field versions[1]: ...".
func Decode(out any, v *Value) error {
	rv := reflect.ValueOf(out)
	if !rv.IsValid() || rv.Kind() != reflect.Pointer || rv.IsNil() {
		return fmt.Errorf("mruby: Decode wants a non-nil pointer, got %T", out)
	}
	return decode(rv.Elem(), v, "")
}

// decode fills the addressable rv from v, path naming where rv sits for an error message.
func decode(rv reflect.Value, v *Value, path string) error {
	if rv.Type() == valueType {
		if v.IsNil() {
			rv.Set(reflect.Zero(rv.Type()))
			return nil
		}
		rv.Set(reflect.ValueOf(v))
		return nil
	}
	if rv.Kind() == reflect.Pointer {
		if v.IsNil() {
			rv.Set(reflect.Zero(rv.Type()))
			return nil
		}
		if rv.IsNil() {
			rv.Set(reflect.New(rv.Type().Elem()))
		}
		return decode(rv.Elem(), v, path)
	}
	if rv.Type() == anyType {
		x, err := v.GoValue()
		if err != nil {
			return fieldErr(path, err)
		}
		if x == nil {
			rv.Set(reflect.Zero(anyType))
			return nil
		}
		rv.Set(reflect.ValueOf(x))
		return nil
	}
	if v.IsNil() {
		return fieldErr(path, fmt.Errorf("mruby: cannot decode nil into a Go %s", rv.Type()))
	}
	if rv.Type() == symbolType {
		sym, err := v.AsSymbol()
		if err != nil {
			return fieldErr(path, err)
		}
		rv.SetString(string(sym))
		return nil
	}

	switch rv.Kind() {
	case reflect.Bool:
		b, err := v.AsBool()
		if err != nil {
			return fieldErr(path, err)
		}
		rv.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := v.AsInt()
		if err != nil {
			return fieldErr(path, err)
		}
		if rv.OverflowInt(n) {
			return fieldErr(path, fmt.Errorf("mruby: %d does not fit in a Go %s", n, rv.Type()))
		}
		rv.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		n, err := v.AsInt()
		if err != nil {
			return fieldErr(path, err)
		}
		if n < 0 || rv.OverflowUint(uint64(n)) {
			return fieldErr(path, fmt.Errorf("mruby: %d does not fit in a Go %s", n, rv.Type()))
		}
		rv.SetUint(uint64(n))
	case reflect.Float32, reflect.Float64:
		f, err := v.AsFloat()
		if err != nil {
			return fieldErr(path, err)
		}
		if rv.OverflowFloat(f) {
			return fieldErr(path, fmt.Errorf("mruby: %v does not fit in a Go %s", f, rv.Type()))
		}
		rv.SetFloat(f)
	case reflect.String:
		switch v.Type() {
		case TypeString:
			s, err := v.AsString()
			if err != nil {
				return fieldErr(path, err)
			}
			rv.SetString(s)
		case TypeSymbol:
			sym, err := v.AsSymbol()
			if err != nil {
				return fieldErr(path, err)
			}
			rv.SetString(string(sym))
		default:
			return fieldErr(path, v.typeError("a String"))
		}
	case reflect.Slice:
		return decodeSlice(rv, v, path)
	case reflect.Map:
		return decodeMap(rv, v, path)
	case reflect.Struct:
		return decodeStruct(rv, v, path)
	default:
		return fieldErr(path, fmt.Errorf("mruby: a Go %s does not decode from Ruby", rv.Type()))
	}
	return nil
}

func decodeSlice(rv reflect.Value, v *Value, path string) error {
	if rv.Type().Elem().Kind() == reflect.Uint8 {
		if v.Type() != TypeString {
			return fieldErr(path, v.typeError("a String"))
		}
		s, err := v.AsString()
		if err != nil {
			return fieldErr(path, err)
		}
		rv.SetBytes([]byte(s))
		return nil
	}
	if v.Type() != TypeArray {
		return fieldErr(path, v.typeError("an Array"))
	}
	lengthV, err := v.Call("length")
	if err != nil {
		return fieldErr(path, err)
	}
	n, err := lengthV.AsInt()
	if err != nil {
		return fieldErr(path, err)
	}
	out := reflect.MakeSlice(rv.Type(), int(n), int(n))
	for i := int64(0); i < n; i++ {
		elem, err := v.Call("[]", i)
		if err != nil {
			return fieldErr(path, err)
		}
		if err := decode(out.Index(int(i)), elem, joinIndex(path, i)); err != nil {
			return err
		}
	}
	rv.Set(out)
	return nil
}

func decodeMap(rv reflect.Value, v *Value, path string) error {
	if v.Type() != TypeHash {
		return fieldErr(path, v.typeError("a Hash"))
	}
	kt := rv.Type().Key()
	if kt.Kind() != reflect.String && kt != anyType {
		return fieldErr(path, fmt.Errorf("mruby: a map with a %s key does not decode from a Hash", kt))
	}
	vt := rv.Type().Elem()

	out := reflect.MakeMap(rv.Type())
	err := hashPairs(v, func(key, val *Value) error {
		elemPath := joinIndex(path, key)

		kv := reflect.New(kt).Elem()
		if err := decode(kv, key, elemPath); err != nil {
			return err
		}
		vv := reflect.New(vt).Elem()
		if err := decode(vv, val, elemPath); err != nil {
			return err
		}
		out.SetMapIndex(kv, vv)
		return nil
	})
	if err != nil {
		return err
	}
	rv.Set(out)
	return nil
}

// structField is one Go struct field ready to be filled: by name, or by squash, which decodes it from the struct's own source instead of a field of that source.
type structField struct {
	index  []int
	name   string
	squash bool
}

func structFieldsOf(t reflect.Type) []structField {
	var fields []structField
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, opts, hasOpts := strings.Cut(f.Tag.Get("mruby"), ",")
		if name == "-" {
			continue
		}
		squash := f.Anonymous && hasOpts && hasOption(opts, "squash")
		if name == "" {
			name = strings.ToLower(f.Name)
		}
		fields = append(fields, structField{index: f.Index, name: name, squash: squash})
	}
	return fields
}

func hasOption(opts, want string) bool {
	for _, o := range strings.Split(opts, ",") {
		if o == want {
			return true
		}
	}
	return false
}

// fieldLookup answers the source value for a struct field name, and whether the source had one.
type fieldLookup func(name string) (*Value, bool, error)

func decodeStruct(rv reflect.Value, v *Value, path string) error {
	lookup, err := fieldLookupFor(v)
	if err != nil {
		return fieldErr(path, err)
	}
	for _, f := range structFieldsOf(rv.Type()) {
		target := rv.FieldByIndex(f.index)
		if f.squash {
			if err := decode(target, v, path); err != nil {
				return err
			}
			continue
		}
		fieldPath := joinField(path, f.name)
		val, ok, err := lookup(f.name)
		if err != nil {
			return fieldErr(fieldPath, err)
		}
		if !ok {
			continue
		}
		if err := decode(target, val, fieldPath); err != nil {
			return err
		}
	}
	return nil
}

// fieldLookupFor is a Hash lookup by key, tried as a String and as a Symbol, for a Hash source;
// a zero-argument method call by name for any other Ruby value, where a NoMethodError means the field has no source rather than an error.
func fieldLookupFor(v *Value) (fieldLookup, error) {
	if v.Type() != TypeHash {
		return func(name string) (*Value, bool, error) {
			val, err := v.Call(name)
			if err != nil {
				var re *RubyError
				if errors.As(err, &re) && re.Class == "NoMethodError" {
					return nil, false, nil
				}
				return nil, false, err
			}
			return val, true, nil
		}, nil
	}

	byKey := map[string]*Value{}
	err := hashPairs(v, func(key, val *Value) error {
		var name string
		switch key.Type() {
		case TypeString:
			s, err := key.AsString()
			if err != nil {
				return err
			}
			name = s
		case TypeSymbol:
			sym, err := key.AsSymbol()
			if err != nil {
				return err
			}
			name = string(sym)
		default:
			return nil
		}
		byKey[name] = val
		return nil
	})
	if err != nil {
		return nil, err
	}
	return func(name string) (*Value, bool, error) {
		val, ok := byKey[name]
		return val, ok, nil
	}, nil
}

// hashPairs visits every key and value of the Hash v.
func hashPairs(v *Value, fn func(key, val *Value) error) error {
	keys, err := v.Call("keys")
	if err != nil {
		return err
	}
	lengthV, err := keys.Call("length")
	if err != nil {
		return err
	}
	n, err := lengthV.AsInt()
	if err != nil {
		return err
	}
	for i := int64(0); i < n; i++ {
		key, err := keys.Call("[]", i)
		if err != nil {
			return err
		}
		val, err := v.Call("[]", key)
		if err != nil {
			return err
		}
		if err := fn(key, val); err != nil {
			return err
		}
	}
	return nil
}

// fieldErr names the field a decode error was found in.
// A path-less error, from the value Decode was given directly, is returned as it is.
func fieldErr(path string, err error) error {
	if path == "" {
		return err
	}
	return fmt.Errorf("field %s: %w", path, err)
}

func joinField(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

func joinIndex(path string, idx any) string {
	return fmt.Sprintf("%s[%v]", path, idx)
}
