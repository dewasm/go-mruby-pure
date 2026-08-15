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

// ToValue converts a Go value into a Ruby one by the conversions in the package documentation, which is the same table an argument crosses by.
// An immediate stays an immediate; an Array or a Hash becomes an object of the interpreter's, which the returned Value holds the reference to.
// A *Value is answered as it is.
func (vm *VM) ToValue(x any) (*Value, error) {
	if v, ok := x.(*Value); ok {
		if _, err := v.arg(vm); err != nil {
			return nil, err
		}
		return v, nil
	}
	defer vm.enter()()

	b := builder{vm: vm}
	defer b.done()

	a, err := b.convert(reflect.ValueOf(x))
	if err != nil {
		return nil, err
	}
	return b.keep(a), nil
}

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
	return b.container(elems, b.vm.in.NewArray, "Array")
}

func (b *builder) hash(rv reflect.Value) (arg, error) {
	pairs := make([]arg, 0, 2*rv.Len())
	for iter := rv.MapRange(); iter.Next(); {
		key, err := b.convert(iter.Key())
		if err != nil {
			return arg{}, err
		}
		value, err := b.convert(iter.Value())
		if err != nil {
			return arg{}, err
		}
		pairs = append(pairs, key, value)
	}
	return b.container(pairs, b.vm.in.NewHash, "Hash")
}

// container has the interpreter build an Array or a Hash out of the pieces, which cross in the argument scratch.
// The reference is the builder's until done gives it back, so nesting one container in another needs no other bookkeeping.
func (b *builder) container(pieces []arg, build func(bool) (mrubyvm.Value, error), name string) (arg, error) {
	if err := b.vm.in.ArgsReset(); err != nil {
		return arg{}, err
	}
	for _, piece := range pieces {
		if err := piece.push(b.vm.in); err != nil {
			return arg{}, err
		}
	}
	raw, err := build(true)
	if err != nil {
		return arg{}, b.vm.wrap(err)
	}
	if raw.Ref == 0 {
		return arg{}, fmt.Errorf("mruby: the interpreter would not hold on to a new %s", name)
	}
	b.temp = append(b.temp, raw.Ref)
	return arg{kind: raw.Kind, ref: raw.Ref}, nil
}

// keep turns a converted argument into a Value that owns what it holds, taking the reference behind it out of the builder's hands.
func (b *builder) keep(a arg) *Value {
	if a.ref == 0 {
		return &Value{vm: b.vm, kind: a.kind, i: a.i, f: a.f, s: a.s}
	}
	for i, ref := range b.temp {
		if ref == a.ref {
			b.temp = append(b.temp[:i], b.temp[i+1:]...)
			break
		}
	}
	return b.vm.handle(a.kind, a.ref)
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
		s, err := v.AsSymbol()
		if err != nil {
			return reflect.Value{}, err
		}
		return reflect.ValueOf(s), nil
	case anyType, anySliceType, anyMapType, stringMapType:
		return vm.fromGoValue(v, t)
	}

	out := reflect.New(t).Elem()
	switch t.Kind() {
	case reflect.Bool:
		b, err := v.AsBool()
		if err != nil {
			return reflect.Value{}, err
		}
		out.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := v.AsInt()
		if err != nil {
			return reflect.Value{}, err
		}
		if out.OverflowInt(n) {
			return reflect.Value{}, fmt.Errorf("mruby: %d does not fit in a Go %s", n, t)
		}
		out.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		n, err := v.AsInt()
		if err != nil {
			return reflect.Value{}, err
		}
		if n < 0 || out.OverflowUint(uint64(n)) {
			return reflect.Value{}, fmt.Errorf("mruby: %d does not fit in a Go %s", n, t)
		}
		out.SetUint(uint64(n))
	case reflect.Float32, reflect.Float64:
		f, err := v.AsFloat()
		if err != nil {
			return reflect.Value{}, err
		}
		if out.OverflowFloat(f) {
			return reflect.Value{}, fmt.Errorf("mruby: %v does not fit in a Go %s", f, t)
		}
		out.SetFloat(f)
	case reflect.String:
		// A Symbol reaches a string parameter as its name; a parameter typed Symbol is what tells the two apart.
		if k := v.Type(); k != TypeString && k != TypeSymbol {
			return reflect.Value{}, v.typeError("a String")
		}
		out.SetString(v.s)
	case reflect.Slice:
		s, err := v.AsString()
		if err != nil {
			return reflect.Value{}, err
		}
		out.SetBytes([]byte(s))
	default:
		return reflect.Value{}, fmt.Errorf("mruby: a Go %s does not cross from Ruby", t)
	}
	return out, nil
}

func (vm *VM) fromGoValue(v *Value, t reflect.Type) (reflect.Value, error) {
	if v == nil {
		return reflect.Zero(t), nil
	}
	x, err := vm.goValue(v, nil)
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
				return reflect.Value{}, fmt.Errorf("mruby: a Hash key that converts to a Go %T does not fit a map[string]any", key)
			}
		}
		return reflect.ValueOf(out), nil
	}
	return reflect.Value{}, fmt.Errorf("mruby: a Go %s does not cross from Ruby", t)
}
