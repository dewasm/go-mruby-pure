package mruby

import (
	"fmt"
	"math"
	"reflect"

	"github.com/dewasm/go-mruby/mrubyvm"
)

// The Go types that cross the boundary, in either direction.
var (
	anyType       = reflect.TypeOf((*any)(nil)).Elem()
	valueType     = reflect.TypeOf((*Value)(nil))
	symbolType    = reflect.TypeOf(Symbol(""))
	anySliceType  = reflect.TypeOf([]any(nil))
	anyMapType    = reflect.TypeOf(map[any]any(nil))
	stringMapType = reflect.TypeOf(map[string]any(nil))
)

// arg is one entry of the interpreter's argument scratch.
type arg struct {
	kind mrubyvm.Kind
	i    int64
	f    float64
	s    string
	ref  int32
}

func (a arg) push(in *mrubyvm.Instance) error {
	switch a.kind {
	case mrubyvm.KindNil:
		return in.PushNil()
	case mrubyvm.KindFalse:
		return in.PushBool(false)
	case mrubyvm.KindTrue:
		return in.PushBool(true)
	case mrubyvm.KindInt:
		return in.PushInt(a.i)
	case mrubyvm.KindFloat:
		return in.PushFloat(a.f)
	case mrubyvm.KindString:
		return in.PushString(a.s)
	case mrubyvm.KindSymbol:
		return in.PushSymbol(a.s)
	}
	return in.PushRef(a.ref)
}

// builder converts Go values into the interpreter's argument scratch.
//
// Every argument is converted before any of them is pushed: an Array or a Hash argument is built by calling into the interpreter, which fills the same scratch.
// The references those calls hand out are the builder's, and done gives them back.
type builder struct {
	vm   *VM
	args []arg
	temp []int32
}

func (b *builder) done() {
	for _, ref := range b.temp {
		b.vm.in.ReleaseRef(ref)
	}
}

// receiver is the reference to call a method on, made on the spot when the value is an immediate.
func (b *builder) receiver(v *Value) (int32, error) {
	a, err := v.arg(b.vm)
	if err != nil {
		return 0, err
	}
	if a.ref != 0 {
		return a.ref, nil
	}
	ref, err := b.vm.materialize(v)
	if err != nil {
		return 0, err
	}
	b.temp = append(b.temp, ref)
	return ref, nil
}

func (b *builder) add(xs ...any) error {
	for _, x := range xs {
		a, err := b.convert(reflect.ValueOf(x))
		if err != nil {
			return err
		}
		b.args = append(b.args, a)
	}
	return nil
}

func (b *builder) flush() error {
	if err := b.vm.in.ArgsReset(); err != nil {
		return err
	}
	for _, a := range b.args {
		if err := a.push(b.vm.in); err != nil {
			return err
		}
	}
	return nil
}

func (b *builder) convert(rv reflect.Value) (arg, error) {
	if !rv.IsValid() {
		return arg{kind: mrubyvm.KindNil}, nil
	}
	switch rv.Kind() {
	case reflect.Interface:
		if rv.IsNil() {
			return arg{kind: mrubyvm.KindNil}, nil
		}
		return b.convert(rv.Elem())
	case reflect.Pointer:
		if rv.Type() == valueType {
			v, _ := rv.Interface().(*Value)
			return v.arg(b.vm)
		}
	}
	switch rv.Type() {
	case symbolType:
		return arg{kind: mrubyvm.KindSymbol, s: rv.String()}, nil
	case anySliceType:
		return b.array(rv)
	case anyMapType, stringMapType:
		return b.hash(rv)
	}
	switch rv.Kind() {
	case reflect.Bool:
		if rv.Bool() {
			return arg{kind: mrubyvm.KindTrue}, nil
		}
		return arg{kind: mrubyvm.KindFalse}, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return arg{kind: mrubyvm.KindInt, i: rv.Int()}, nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		n := rv.Uint()
		if n > math.MaxInt64 {
			return arg{}, fmt.Errorf("mruby: %d is past what a Ruby Integer holds", n)
		}
		return arg{kind: mrubyvm.KindInt, i: int64(n)}, nil
	case reflect.Float32, reflect.Float64:
		return arg{kind: mrubyvm.KindFloat, f: rv.Float()}, nil
	case reflect.String:
		return arg{kind: mrubyvm.KindString, s: rv.String()}, nil
	case reflect.Slice:
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			return arg{kind: mrubyvm.KindString, s: string(rv.Bytes())}, nil
		}
	}
	return arg{}, fmt.Errorf("mruby: a Go %s does not cross into Ruby", rv.Type())
}

func (b *builder) array(rv reflect.Value) (arg, error) {
	elems := make([]arg, rv.Len())
	for i := range elems {
		a, err := b.convert(rv.Index(i))
		if err != nil {
			return arg{}, err
		}
		elems[i] = a
	}
	ref, err := b.instance(&b.vm.arrayClass, "Array")
	if err != nil {
		return arg{}, err
	}
	if len(elems) > 0 {
		if err := b.vm.in.ArgsReset(); err != nil {
			return arg{}, err
		}
		for _, a := range elems {
			if err := a.push(b.vm.in); err != nil {
				return arg{}, err
			}
		}
		if _, err := b.vm.in.Call(ref, "push", false); err != nil {
			return arg{}, b.vm.wrap(err)
		}
	}
	return arg{kind: mrubyvm.KindArray, ref: ref}, nil
}

func (b *builder) hash(rv reflect.Value) (arg, error) {
	type pair struct{ key, value arg }
	pairs := make([]pair, 0, rv.Len())
	for iter := rv.MapRange(); iter.Next(); {
		key, err := b.convert(iter.Key())
		if err != nil {
			return arg{}, err
		}
		value, err := b.convert(iter.Value())
		if err != nil {
			return arg{}, err
		}
		pairs = append(pairs, pair{key, value})
	}
	ref, err := b.instance(&b.vm.hashClass, "Hash")
	if err != nil {
		return arg{}, err
	}
	for _, p := range pairs {
		if err := b.vm.in.ArgsReset(); err != nil {
			return arg{}, err
		}
		if err := p.key.push(b.vm.in); err != nil {
			return arg{}, err
		}
		if err := p.value.push(b.vm.in); err != nil {
			return arg{}, err
		}
		if _, err := b.vm.in.Call(ref, "[]=", false); err != nil {
			return arg{}, b.vm.wrap(err)
		}
	}
	return arg{kind: mrubyvm.KindHash, ref: ref}, nil
}

// instance makes an empty container of a core class, keeping the class reference for the next time.
func (b *builder) instance(cached *int32, name string) (int32, error) {
	class, err := b.vm.constant(cached, name)
	if err != nil {
		return 0, err
	}
	if err := b.vm.in.ArgsReset(); err != nil {
		return 0, err
	}
	raw, err := b.vm.in.Call(class, "new", true)
	if err != nil {
		return 0, b.vm.wrap(err)
	}
	if raw.Ref == 0 {
		return 0, fmt.Errorf("mruby: the interpreter would not hold on to a new %s", name)
	}
	b.temp = append(b.temp, raw.Ref)
	return raw.Ref, nil
}

// crosses reports whether a Go type is one this package converts, which is what a host function's parameters and result are restricted to.
func crosses(t reflect.Type) bool {
	switch t {
	case anyType, valueType, symbolType, anySliceType, anyMapType, stringMapType:
		return true
	}
	switch t.Kind() {
	case reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64,
		reflect.String:
		return true
	case reflect.Slice:
		return t.Elem().Kind() == reflect.Uint8
	}
	return false
}

// fromValue converts a Ruby value to the Go type a host function's parameter asks for.
func (vm *VM) fromValue(v *Value, t reflect.Type) (reflect.Value, error) {
	switch t {
	case valueType:
		if v == nil {
			return reflect.Zero(t), nil
		}
		return reflect.ValueOf(v), nil
	case symbolType:
		if v.Type() != TypeSymbol {
			return reflect.Value{}, v.typeError("a Symbol")
		}
		return reflect.ValueOf(Symbol(v.s)), nil
	case anyType, anySliceType, anyMapType, stringMapType:
		return vm.fromExported(v, t)
	}

	out := reflect.New(t).Elem()
	switch t.Kind() {
	case reflect.Bool:
		b, err := v.Bool()
		if err != nil {
			return reflect.Value{}, err
		}
		out.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := v.Int()
		if err != nil {
			return reflect.Value{}, err
		}
		if out.OverflowInt(n) {
			return reflect.Value{}, fmt.Errorf("mruby: %d does not fit in a Go %s", n, t)
		}
		out.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		n, err := v.Int()
		if err != nil {
			return reflect.Value{}, err
		}
		if n < 0 || out.OverflowUint(uint64(n)) {
			return reflect.Value{}, fmt.Errorf("mruby: %d does not fit in a Go %s", n, t)
		}
		out.SetUint(uint64(n))
	case reflect.Float32, reflect.Float64:
		f, err := v.Float()
		if err != nil {
			return reflect.Value{}, err
		}
		if out.OverflowFloat(f) {
			return reflect.Value{}, fmt.Errorf("mruby: %v does not fit in a Go %s", f, t)
		}
		out.SetFloat(f)
	case reflect.String:
		s, err := v.Text()
		if err != nil {
			return reflect.Value{}, err
		}
		out.SetString(s)
	case reflect.Slice:
		if v.Type() != TypeString {
			return reflect.Value{}, v.typeError("a String")
		}
		out.SetBytes([]byte(v.s))
	default:
		return reflect.Value{}, fmt.Errorf("mruby: a Go %s does not cross from Ruby", t)
	}
	return out, nil
}

func (vm *VM) fromExported(v *Value, t reflect.Type) (reflect.Value, error) {
	if v == nil {
		return reflect.Zero(t), nil
	}
	x, err := vm.export(v, nil)
	if err != nil {
		return reflect.Value{}, err
	}
	switch t {
	case anyType:
		if x == nil {
			return reflect.Zero(anyType), nil
		}
		return reflect.ValueOf(x), nil
	case anySliceType:
		xs, ok := x.([]any)
		if !ok {
			return reflect.Value{}, v.typeError("an Array")
		}
		return reflect.ValueOf(xs), nil
	case anyMapType:
		m, ok := x.(map[any]any)
		if !ok {
			return reflect.Value{}, v.typeError("a Hash")
		}
		return reflect.ValueOf(m), nil
	case stringMapType:
		m, ok := x.(map[any]any)
		if !ok {
			return reflect.Value{}, v.typeError("a Hash")
		}
		out := make(map[string]any, len(m))
		for key, value := range m {
			switch key := key.(type) {
			case string:
				out[key] = value
			case Symbol:
				out[string(key)] = value
			default:
				return reflect.Value{}, fmt.Errorf("mruby: a Hash key that exports as %T does not fit a Go map[string]any", key)
			}
		}
		return reflect.ValueOf(out), nil
	}
	return reflect.Value{}, fmt.Errorf("mruby: a Go %s does not cross from Ruby", t)
}
