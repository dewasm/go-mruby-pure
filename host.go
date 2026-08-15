package mruby

import (
	"errors"
	"fmt"
	"reflect"
	"runtime/debug"
	"strings"

	"github.com/dewasm/go-mruby/mrubyvm"
)

// Define defines name as a Ruby method callable from anywhere, an instance method of Kernel.
//
// fn is either a plain Go func, whose parameters and result cross by the conversions in the package documentation and whose arity Ruby enforces as an ArgumentError, or a func(*Call) (any, error), which receives the call itself.
func (vm *VM) Define(name string, fn any) error {
	defer vm.enter()()

	id, err := vm.register(name, fn)
	if err != nil {
		return err
	}
	if err := vm.in.DefineMethod(0, name, id); err != nil {
		return vm.wrap(err)
	}
	return nil
}

// DefineClass defines name as a class under Object, deriving from super, or from Object when super is nil.
func (vm *VM) DefineClass(name string, super *Class) (*Class, error) {
	defer vm.enter()()

	var superRef int32
	if super != nil {
		if super.vm != vm {
			return nil, errors.New("mruby: the superclass belongs to another VM")
		}
		superRef = super.value.ref
	}
	ref, err := vm.in.DefineClass(name, superRef)
	if err != nil {
		return nil, vm.wrap(err)
	}
	if ref == 0 {
		return nil, fmt.Errorf("mruby: the interpreter would not hold on to class %s", name)
	}
	return &Class{vm: vm, name: name, value: vm.handle(mrubyvm.KindOther, ref)}, nil
}

// Class is a Ruby class defined from Go.
type Class struct {
	vm    *VM
	name  string
	value *Value
}

// Value is the class object itself, for sending it a message or passing it on.
func (c *Class) Value() *Value {
	return c.value
}

// DefineMethod defines name as an instance method of the class, taking the same fn as VM.Define.
func (c *Class) DefineMethod(name string, fn any) error {
	defer c.vm.enter()()

	id, err := c.vm.register(c.name+"#"+name, fn)
	if err != nil {
		return err
	}
	if err := c.vm.in.DefineMethod(c.value.ref, name, id); err != nil {
		return c.vm.wrap(err)
	}
	return nil
}

// New makes an instance, passing the arguments on to initialize.
func (c *Class) New(args ...any) (*Value, error) {
	defer c.vm.enter()()
	return c.vm.call(c.value, "new", args)
}

func (vm *VM) register(name string, fn any) (int32, error) {
	h, err := makeHostFunc(name, fn)
	if err != nil {
		return 0, fmt.Errorf("mruby: %s: %w", name, err)
	}
	vm.fns = append(vm.fns, h)
	return int32(len(vm.fns)), nil
}

// Call is the Ruby call a host function in explicit form is running inside.
// It is valid until that function returns, and only on the goroutine running it.
type Call struct {
	vm    *VM
	count int

	self       *Value
	selfTaken  bool
	block      *Value
	blockTaken bool
	args       []*Value
}

// VM is the interpreter the call is running in.
func (c *Call) VM() *VM {
	return c.vm
}

// Len is how many arguments the Ruby caller passed.
func (c *Call) Len() int {
	return c.count
}

// Arg is argument i, counted from zero, and nil when the caller passed no such argument.
func (c *Call) Arg(i int) *Value {
	if i < 0 || i >= c.count {
		return nil
	}
	if c.args == nil {
		c.args = make([]*Value, c.count)
	}
	if c.args[i] == nil {
		c.args[i] = c.vm.hostArg(int32(i))
	}
	return c.args[i]
}

// Self is the receiver of the call.
func (c *Call) Self() *Value {
	if !c.selfTaken {
		c.selfTaken = true
		c.self = c.vm.adopt(c.vm.in.HostSelfRef())
	}
	return c.self
}

// Block is the block the Ruby caller passed, nil when it passed none.
// Calling it is Call("call", ...) on the Proc.
func (c *Call) Block() *Value {
	if !c.blockTaken {
		c.blockTaken = true
		c.block = c.vm.adopt(c.vm.in.HostBlockRef())
	}
	return c.block
}

// hostArg reads one argument of the call now running.
func (vm *VM) hostArg(i int32) *Value {
	in := vm.in
	kind := in.HostArgKind(i)
	switch kind {
	case mrubyvm.KindTrue, mrubyvm.KindFalse:
		return &Value{vm: vm, kind: kind}
	case mrubyvm.KindInt:
		return &Value{vm: vm, kind: kind, i: in.HostArgInt(i)}
	case mrubyvm.KindFloat:
		return &Value{vm: vm, kind: kind, f: in.HostArgFloat(i)}
	case mrubyvm.KindString, mrubyvm.KindSymbol:
		return &Value{vm: vm, kind: kind, s: in.HostArgString(i)}
	case mrubyvm.KindArray, mrubyvm.KindHash, mrubyvm.KindOther:
		if ref := in.HostArgRef(i); ref != 0 {
			return vm.handle(kind, ref)
		}
	}
	return &Value{vm: vm, kind: mrubyvm.KindNil}
}

// adopt takes over a reference the interpreter just handed out, reading an immediate out of it so that the reference goes back at once.
func (vm *VM) adopt(ref int32) *Value {
	if ref == 0 {
		return nil
	}
	kind, err := vm.in.RefKind(ref)
	if err != nil || kind == mrubyvm.KindInvalid {
		vm.in.ReleaseRef(ref)
		return nil
	}
	if referenced(kind) {
		return vm.handle(kind, ref)
	}
	defer vm.in.ReleaseRef(ref)

	if err := vm.in.ArgsReset(); err != nil {
		return nil
	}
	if err := vm.in.PushRef(ref); err != nil {
		return nil
	}
	raw, err := vm.in.CaptureArg(false)
	if err != nil {
		return nil
	}
	v, err := vm.capture(raw)
	if err != nil {
		return nil
	}
	return v
}

// dispatch is the one callback the interpreter has: it runs the Go function the Ruby method was defined with.
func (vm *VM) dispatch(*mrubyvm.Instance) int32 {
	vm.hostDepth.Add(1)
	defer vm.hostDepth.Add(-1)

	id := vm.in.HostFnID()
	if id < 1 || int(id) > len(vm.fns) {
		vm.in.HostRaise("RuntimeError", fmt.Sprintf("mruby: no Go function is registered under id %d", id))
		return 1
	}
	fn := vm.fns[id-1]

	out, err := vm.invoke(fn, &Call{vm: vm, count: int(vm.in.HostArgsCount())})
	if err == nil {
		err = vm.result(out)
	}
	if err != nil {
		vm.raise(err)
		return 1
	}
	return 0
}

// invoke runs one host function, turning a panic into a Ruby exception.
// A panic must not unwind through the interpreter's frames, which would abandon its stacks; the panics the generated code raises are the exception, since no Ruby exception can stand in for them.
func (vm *VM) invoke(fn *hostFunc, c *Call) (out any, err error) {
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		if mrubyvm.GuestError(r) != nil {
			panic(r)
		}
		err = fmt.Errorf("mruby: %s panicked: %v\n\n%s", fn.name, r, debug.Stack())
	}()
	return fn.call(c)
}

// result leaves what the host function returned where the interpreter takes it from.
func (vm *VM) result(out any) error {
	b := builder{vm: vm}
	defer b.done()

	if err := b.add(out); err != nil {
		return err
	}
	return b.flush()
}

func (vm *VM) raise(err error) {
	var re *RubyError
	if errors.As(err, &re) && re.Class != "" {
		vm.in.HostRaise(re.Class, re.Message)
		return
	}
	vm.in.HostRaise("RuntimeError", err.Error())
}

// hostFunc is a Go function reachable from Ruby, reduced to the one shape dispatch calls.
type hostFunc struct {
	name string
	call func(*Call) (any, error)
}

var (
	callType     = reflect.TypeOf((*Call)(nil))
	errorType    = reflect.TypeOf((*error)(nil)).Elem()
	explicitType = reflect.TypeOf((func(*Call) (any, error))(nil))
)

func makeHostFunc(name string, fn any) (*hostFunc, error) {
	rv := reflect.ValueOf(fn)
	if !rv.IsValid() || rv.Kind() != reflect.Func {
		return nil, fmt.Errorf("a host function is a Go func, not %T", fn)
	}
	if rv.IsNil() {
		return nil, errors.New("a host function is not a nil func")
	}
	t := rv.Type()
	if !t.IsVariadic() && t.NumIn() == 1 && t.In(0) == callType &&
		t.NumOut() == 2 && t.Out(0) == anyType && t.Out(1) == errorType {
		call, _ := rv.Convert(explicitType).Interface().(func(*Call) (any, error))
		return &hostFunc{name: name, call: call}, nil
	}
	return reflectionForm(name, rv)
}

// reflectionForm calls a plain Go func with the Ruby arguments converted to its parameters, and converts its result back.
func reflectionForm(name string, rv reflect.Value) (*hostFunc, error) {
	t := rv.Type()
	fixed := t.NumIn()
	if t.IsVariadic() {
		fixed--
	}
	for i := 0; i < t.NumIn(); i++ {
		pt := t.In(i)
		if t.IsVariadic() && i == fixed {
			pt = pt.Elem()
		}
		if !crosses(pt) {
			return nil, fmt.Errorf("parameter %d is a %s, which does not cross from Ruby", i+1, pt)
		}
	}

	var hasResult, hasError bool
	switch t.NumOut() {
	case 0:
	case 1:
		hasError = t.Out(0) == errorType
		hasResult = !hasError
	case 2:
		if t.Out(1) != errorType {
			return nil, fmt.Errorf("the second result is a %s, which is not error", t.Out(1))
		}
		hasResult, hasError = true, true
	default:
		return nil, fmt.Errorf("a host function returns nothing, a value, an error, or a value and an error, not %d results", t.NumOut())
	}
	if hasResult && !crosses(t.Out(0)) {
		return nil, fmt.Errorf("the result is a %s, which does not cross into Ruby", t.Out(0))
	}

	call := func(c *Call) (any, error) {
		if c.Len() < fixed || (!t.IsVariadic() && c.Len() > fixed) {
			return nil, arityError(c.Len(), fixed, t.IsVariadic())
		}
		in := make([]reflect.Value, c.Len())
		for i := range in {
			pt := t.In(t.NumIn() - 1)
			if i < fixed {
				pt = t.In(i)
			} else {
				pt = pt.Elem()
			}
			gv, err := c.vm.fromValue(c.Arg(i), pt)
			if err != nil {
				return nil, Raise("TypeError", "argument %d: %s", i+1, rubyMessage(err))
			}
			in[i] = gv
		}
		out := rv.Call(in)
		if hasError {
			if err, _ := out[len(out)-1].Interface().(error); err != nil {
				return nil, err
			}
		}
		if hasResult {
			return out[0].Interface(), nil
		}
		return nil, nil
	}
	return &hostFunc{name: name, call: call}, nil
}

func arityError(got, want int, variadic bool) error {
	if variadic {
		return Raise("ArgumentError", "wrong number of arguments (given %d, expected %d+)", got, want)
	}
	return Raise("ArgumentError", "wrong number of arguments (given %d, expected %d)", got, want)
}

// rubyMessage states a Go error the way a Ruby message states it, without this package's prefix.
func rubyMessage(err error) string {
	return strings.TrimPrefix(err.Error(), "mruby: ")
}
