package mruby

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/dewasm/go-mruby-pure/mrubyvm"
)

// Type is what Ruby class a Value belongs to, as far as this package distinguishes.
type Type int

const (
	TypeNil Type = iota
	TypeBool
	TypeInteger
	TypeFloat
	TypeString
	TypeSymbol
	TypeArray
	TypeHash
	// TypeObject is everything else: an instance of a class this package has no conversion for, including a Proc.
	TypeObject
)

func (t Type) String() string {
	switch t {
	case TypeNil:
		return "nil"
	case TypeBool:
		return "true or false"
	case TypeInteger:
		return "Integer"
	case TypeFloat:
		return "Float"
	case TypeString:
		return "String"
	case TypeSymbol:
		return "Symbol"
	case TypeArray:
		return "Array"
	case TypeHash:
		return "Hash"
	case TypeObject:
		return "object"
	}
	return "unknown"
}

// Symbol is a Ruby Symbol. It crosses into Ruby as one, and a Symbol comes back as one.
type Symbol string

// Value is a Ruby value held from Go.
//
// nil, true, false, an Integer, a Float, a String and a Symbol are copied when the Value is made and need nothing from the interpreter afterwards.
// Every other value is a reference into the interpreter, which the collector gives back when the Value becomes unreachable; Release gives it back at once.
//
// A nil *Value reads as Ruby nil, which is what a missing argument or an absent block is.
type Value struct {
	vm   *VM
	kind mrubyvm.Kind

	i int64
	f float64
	s string

	// Zero for an immediate: the kinds that carry a reference are exactly the ones referenced reports.
	ref      int32
	cleanup  runtime.Cleanup
	released atomic.Bool
}

// held is what a cleanup needs to give a reference back, kept apart from the Value so that the Value stays collectable.
type held struct {
	in  *mrubyvm.Instance
	ref int32
}

// Anything that reads v.ref and then enters the interpreter must keep v reachable past the entry (runtime.KeepAlive): once v is unreachable this cleanup queues the reference, and the next entry point, on any goroutine, frees it.
func addCleanup(v *Value, h held) runtime.Cleanup {
	return runtime.AddCleanup(v, func(h held) { h.in.ReleaseRef(h.ref) }, h)
}

var errReleased = errors.New("mruby: the value has been released")

// Type is the Ruby type of the value.
func (v *Value) Type() Type {
	if v == nil {
		return TypeNil
	}
	switch v.kind {
	case mrubyvm.KindFalse, mrubyvm.KindTrue:
		return TypeBool
	case mrubyvm.KindInt:
		return TypeInteger
	case mrubyvm.KindFloat:
		return TypeFloat
	case mrubyvm.KindString:
		return TypeString
	case mrubyvm.KindSymbol:
		return TypeSymbol
	case mrubyvm.KindArray:
		return TypeArray
	case mrubyvm.KindHash:
		return TypeHash
	case mrubyvm.KindOther:
		return TypeObject
	}
	return TypeNil
}

// IsNil reports whether the value is Ruby nil. False is not nil, as in Ruby.
func (v *Value) IsNil() bool {
	return v == nil || v.kind == mrubyvm.KindNil
}

// AsInt is the value of an Integer.
func (v *Value) AsInt() (int64, error) {
	if v == nil || v.kind != mrubyvm.KindInt {
		return 0, v.typeError("an Integer")
	}
	return v.i, nil
}

// AsFloat is the value of a Float, or of an Integer widened to one.
func (v *Value) AsFloat() (float64, error) {
	if v == nil {
		return 0, v.typeError("a Float")
	}
	switch v.kind {
	case mrubyvm.KindFloat:
		return v.f, nil
	case mrubyvm.KindInt:
		return float64(v.i), nil
	}
	return 0, v.typeError("a Float")
}

// AsBool is the value of true or false. Every other value, nil included, is an error rather than Ruby's truthiness.
func (v *Value) AsBool() (bool, error) {
	if v == nil {
		return false, v.typeError("true or false")
	}
	switch v.kind {
	case mrubyvm.KindTrue:
		return true, nil
	case mrubyvm.KindFalse:
		return false, nil
	}
	return false, v.typeError("true or false")
}

// AsString is the bytes of a String. A Symbol is not one; [Value.AsSymbol] takes that.
func (v *Value) AsString() (string, error) {
	if v == nil || v.kind != mrubyvm.KindString {
		return "", v.typeError("a String")
	}
	return v.s, nil
}

// AsSymbol is the name of a Symbol.
func (v *Value) AsSymbol() (Symbol, error) {
	if v == nil || v.kind != mrubyvm.KindSymbol {
		return "", v.typeError("a Symbol")
	}
	return Symbol(v.s), nil
}

func (v *Value) typeError(want string) error {
	return fmt.Errorf("mruby: the value is %s, not %s", v.Type(), want)
}

// String is the value's Ruby to_s, which Ruby code may override; the bytes of a String are read with [Value.AsString].
// It is best effort, for messages and debugging: an exception raised on the way out becomes the returned text.
func (v *Value) String() string {
	if v == nil {
		return ""
	}
	switch v.kind {
	case mrubyvm.KindNil:
		return ""
	case mrubyvm.KindFalse:
		return "false"
	case mrubyvm.KindTrue:
		return "true"
	case mrubyvm.KindInt:
		return strconv.FormatInt(v.i, 10)
	case mrubyvm.KindFloat:
		return formatFloat(v.f)
	case mrubyvm.KindString, mrubyvm.KindSymbol:
		return v.s
	}
	// Nothing inside this package formats a Value: this takes the interpreter's lock, which the caller may already hold.
	defer v.vm.enter()()
	defer runtime.KeepAlive(v)
	if v.released.Load() {
		return errReleased.Error()
	}
	s, err := v.vm.callString(v.ref, "to_s")
	if err != nil {
		return err.Error()
	}
	return s
}

// Inspect is the value's Ruby inspect, best effort like String.
func (v *Value) Inspect() string {
	if v == nil {
		return "nil"
	}
	switch v.kind {
	case mrubyvm.KindNil:
		return "nil"
	case mrubyvm.KindFalse:
		return "false"
	case mrubyvm.KindTrue:
		return "true"
	case mrubyvm.KindInt:
		return strconv.FormatInt(v.i, 10)
	case mrubyvm.KindFloat:
		return formatFloat(v.f)
	}
	defer v.vm.enter()()
	defer runtime.KeepAlive(v)
	if v.released.Load() {
		return errReleased.Error()
	}
	ref := v.ref
	if ref == 0 {
		// A String and a Symbol are quoted and escaped by Ruby's rules, which only Ruby knows.
		temp, err := v.vm.materialize(v)
		if err != nil {
			return err.Error()
		}
		defer v.vm.in.ReleaseRef(temp)
		ref = temp
	}
	s, err := v.vm.callString(ref, "inspect")
	if err != nil {
		return err.Error()
	}
	return s
}

// Call invokes the method name on the value.
func (v *Value) Call(name string, args ...any) (*Value, error) {
	if v == nil {
		return nil, errors.New("mruby: Call on a value that is not there")
	}
	defer v.vm.enter()()
	return v.vm.call(v, name, args)
}

// CallWithBlock is Call with block passed as the method's block, which is what a Ruby yield reaches; a nil block passes none.
func (v *Value) CallWithBlock(name string, block *Value, args ...any) (*Value, error) {
	if v == nil {
		return nil, errors.New("mruby: CallWithBlock on a value that is not there")
	}
	defer v.vm.enter()()
	return v.vm.callBlock(v, name, block, args)
}

// Release gives the interpreter its reference back without waiting for the collector.
// The value is unusable afterwards. Releasing twice, or releasing an immediate, does nothing.
func (v *Value) Release() {
	if v == nil || v.ref == 0 {
		return
	}
	defer runtime.KeepAlive(v)
	if !v.released.CompareAndSwap(false, true) {
		return
	}
	v.cleanup.Stop()
	v.vm.in.ReleaseRef(v.ref)
}

// GoValue converts the value to Go: nil, bool, int64, float64, string, Symbol, []any for an Array and map[any]any for a Hash, recursively.
// A value with no Go counterpart stays a *Value where it sits, and so does a Hash key that Go cannot use as a map key.
// A structure that contains itself is an error rather than an endless walk.
func (v *Value) GoValue() (any, error) {
	if v == nil {
		return nil, nil
	}
	defer v.vm.enter()()
	defer runtime.KeepAlive(v)
	return v.vm.goValue(v, nil)
}

func (vm *VM) goValue(v *Value, path []int64) (any, error) {
	switch v.kind {
	case mrubyvm.KindNil:
		return nil, nil
	case mrubyvm.KindFalse:
		return false, nil
	case mrubyvm.KindTrue:
		return true, nil
	case mrubyvm.KindInt:
		return v.i, nil
	case mrubyvm.KindFloat:
		return v.f, nil
	case mrubyvm.KindString:
		return v.s, nil
	case mrubyvm.KindSymbol:
		return Symbol(v.s), nil
	}
	if v.released.Load() {
		return nil, errReleased
	}
	if v.vm != vm {
		return nil, errors.New("mruby: the value belongs to another VM")
	}
	switch v.kind {
	case mrubyvm.KindArray:
		return vm.goSlice(v, path)
	case mrubyvm.KindHash:
		return vm.goMap(v, path)
	}
	return v, nil
}

// descend adds v to the path being walked, or reports the cycle that adding it would close.
func (vm *VM) descend(v *Value, path []int64) ([]int64, error) {
	id, err := vm.objectID(v.ref)
	if err != nil {
		return nil, err
	}
	for _, seen := range path {
		if seen == id {
			return nil, fmt.Errorf("mruby: cannot convert the %s with object id %d: it contains itself", v.Type(), id)
		}
	}
	return append(path[:len(path):len(path)], id), nil
}

func (vm *VM) goSlice(v *Value, path []int64) (any, error) {
	defer runtime.KeepAlive(v)
	path, err := vm.descend(v, path)
	if err != nil {
		return nil, err
	}
	n, err := vm.in.ArrayLen(v.ref)
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, n)
	for i := int64(0); i < n; i++ {
		raw, err := vm.in.ArrayGet(v.ref, i, false)
		if err != nil {
			return nil, vm.wrap(err)
		}
		elem, err := vm.capture(raw)
		if err != nil {
			return nil, err
		}
		x, err := vm.goValue(elem, path)
		if err != nil {
			return nil, err
		}
		releaseUnless(elem, x)
		out = append(out, x)
	}
	return out, nil
}

func (vm *VM) goMap(v *Value, path []int64) (any, error) {
	defer runtime.KeepAlive(v)
	path, err := vm.descend(v, path)
	if err != nil {
		return nil, err
	}
	keys, err := vm.in.HashKeys(v.ref)
	if err != nil {
		return nil, vm.wrap(err)
	}
	defer vm.in.ReleaseRef(keys)

	n, err := vm.in.ArrayLen(keys)
	if err != nil {
		return nil, err
	}
	out := make(map[any]any, n)
	for i := int64(0); i < n; i++ {
		// The key crosses as a reference whatever its kind is: looking a value up needs one.
		kraw, err := vm.in.ArrayGet(keys, i, true)
		if err != nil {
			return nil, vm.wrap(err)
		}
		vraw, err := vm.in.HashGet(v.ref, kraw.Ref, false)
		if err != nil {
			return nil, vm.wrap(err)
		}
		value, err := vm.capture(vraw)
		if err != nil {
			return nil, err
		}
		var key *Value
		if referenced(kraw.Kind) {
			key = vm.handle(kraw.Kind, kraw.Ref)
		} else {
			key = &Value{vm: vm, kind: kraw.Kind, i: kraw.Int, f: kraw.Float, s: kraw.Str}
			vm.in.ReleaseRef(kraw.Ref)
		}

		ek, err := vm.goValue(key, path)
		if err != nil {
			return nil, err
		}
		ev, err := vm.goValue(value, path)
		if err != nil {
			return nil, err
		}
		if ek != nil && !reflect.TypeOf(ek).Comparable() {
			ek = key
		}
		releaseUnless(key, ek)
		releaseUnless(value, ev)
		out[ek] = ev
	}
	return out, nil
}

// releaseUnless drops the reference behind a value that the converted tree did not keep.
func releaseUnless(v *Value, converted any) {
	if kept, ok := converted.(*Value); ok && kept == v {
		return
	}
	v.Release()
}

// arg is the value ready to go back into the interpreter, as an immediate or as the reference it already holds.
func (v *Value) arg(vm *VM) (arg, error) {
	if v == nil {
		return arg{kind: mrubyvm.KindNil}, nil
	}
	if v.vm != vm {
		return arg{}, errors.New("mruby: the value belongs to another VM")
	}
	if v.released.Load() {
		return arg{}, errReleased
	}
	return arg{kind: v.kind, i: v.i, f: v.f, s: v.s, ref: v.ref}, nil
}

// formatFloat is mruby's Float#to_s: fifteen significant digits, and a fractional part even where there is none (src/numeric.c, mrb_float_to_str).
func formatFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	s := strconv.FormatFloat(f, 'g', 15, 64)
	mantissa, exponent, scientific := strings.Cut(s, "e")
	if !strings.Contains(mantissa, ".") {
		mantissa += ".0"
	}
	if !scientific {
		return mantissa
	}
	return mantissa + "e" + exponent
}
