// Package mruby runs Ruby code from Go.
//
// The mruby interpreter is compiled to WebAssembly and translated to Go source, so a VM is an ordinary Go value: no cgo, no shared library, nothing to close.
//
//	vm, err := mruby.New()
//	v, err := vm.Eval("1 + 2")
//	n, err := v.Int()
//
// # Conversions
//
// A closed list of Go types crosses into Ruby, as an argument, as a host function's result, or as an element of either:
// nil, bool, every signed and unsigned integer type (a uint64 past [math.MaxInt64] is an error), float32 and float64,
// string, [Symbol], []byte as a String, []any as an Array, map[any]any and map[string]any as a Hash, and [Value].
// Anything else is an error naming the Go type.
//
// Coming back, [Value.Export] answers nil, bool, int64, float64, string, [Symbol], []any and map[any]any,
// and leaves a *[Value] where a Ruby value has no Go counterpart.
package mruby

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/dewasm/go-mruby/mrubyvm"
)

// VM is one Ruby interpreter with its own global state, its own object space and its own output.
//
// Every method is safe to call from several goroutines: they serialize on the interpreter.
// The one exception is a host function: while one is running, only the goroutine running it may use its VM or any Value belonging to it.
//
// A VM owns no operating system resources. Drop the last reference to it and the collector takes it; there is nothing to close.
type VM struct {
	in *mrubyvm.Instance

	mu sync.Mutex
	// One entry per host function in flight, so a call the host makes back into Ruby skips a lock its own goroutine already holds.
	hostDepth atomic.Int32

	// Indexed by the id passed to the interpreter, offset by one so that id 0 stays "no function".
	fns []*hostFunc

	// References to the interpreter's own side of the conversions, evaluated on first use and held for the life of the VM.
	identity   int32
	arrayClass int32
	hashClass  int32
}

// Option configures a VM.
type Option func(*options)

type options struct {
	stdout io.Writer
	stderr io.Writer
}

// WithStdout sends what Ruby prints to w instead of os.Stdout; a nil w discards it.
func WithStdout(w io.Writer) Option {
	return func(o *options) { o.stdout = w }
}

// WithStderr sends what Ruby writes to standard error to w instead of os.Stderr; a nil w discards it.
func WithStderr(w io.Writer) Option {
	return func(o *options) { o.stderr = w }
}

// New starts an interpreter.
func New(opts ...Option) (*VM, error) {
	o := options{stdout: os.Stdout, stderr: os.Stderr}
	for _, opt := range opts {
		opt(&o)
	}
	vm := &VM{}
	in, err := mrubyvm.NewInstance(o.stdout, o.stderr, vm.dispatch)
	if err != nil {
		return nil, err
	}
	vm.in = in
	return vm, nil
}

// Eval compiles and runs src.
// One VM keeps one compiler context, so a local variable defined by one Eval is visible to the next.
func (vm *VM) Eval(src string) (*Value, error) {
	defer vm.enter()()

	raw, err := vm.in.Eval(src, false)
	if err != nil {
		return nil, vm.wrap(err)
	}
	return vm.capture(raw)
}

// Call invokes the method name on recv, or on the top-level object when recv is nil.
// The arguments cross by the conversions in the package documentation.
func (vm *VM) Call(recv *Value, name string, args ...any) (*Value, error) {
	defer vm.enter()()
	return vm.call(recv, name, args)
}

// enter serializes calls into the interpreter and returns the matching leave.
// A call made from inside a host function runs on the goroutine that already holds the lock, so it must not take it again.
func (vm *VM) enter() func() {
	if vm.hostDepth.Load() > 0 {
		return func() {}
	}
	vm.mu.Lock()
	return vm.mu.Unlock
}

func (vm *VM) call(recv *Value, name string, args []any) (*Value, error) {
	b := builder{vm: vm}
	defer b.done()

	var recvRef int32
	if recv != nil {
		var err error
		if recvRef, err = b.receiver(recv); err != nil {
			return nil, err
		}
	}
	if err := b.add(args...); err != nil {
		return nil, err
	}
	if err := b.flush(); err != nil {
		return nil, err
	}
	raw, err := vm.in.Call(recvRef, name, false)
	if err != nil {
		return nil, vm.wrap(err)
	}
	return vm.capture(raw)
}

// capture turns one readout of the interpreter's last result into a Value: an immediate carries its data, everything else a reference.
func (vm *VM) capture(raw mrubyvm.Value) (*Value, error) {
	if !referenced(raw.Kind) {
		return &Value{vm: vm, kind: raw.Kind, i: raw.Int, f: raw.Float, s: raw.Str}, nil
	}
	ref, err := vm.in.ResultRef()
	if err != nil {
		return nil, err
	}
	if ref == 0 {
		return nil, errors.New("mruby: the interpreter would not hold on to the result")
	}
	return vm.handle(raw.Kind, ref), nil
}

func (vm *VM) handle(kind mrubyvm.Kind, ref int32) *Value {
	v := &Value{vm: vm, kind: kind, ref: ref}
	v.cleanup = addCleanup(v, held{in: vm.in, ref: ref})
	return v
}

// wrap restates a Ruby exception in this package's terms and leaves every other error as it is.
func (vm *VM) wrap(err error) error {
	var re *mrubyvm.RubyError
	if !errors.As(err, &re) {
		return err
	}
	var backtrace []string
	if re.Backtrace != "" {
		backtrace = strings.Split(re.Backtrace, "\n")
	}
	return &RubyError{Class: re.Class, Message: re.Message, Backtrace: backtrace}
}

// callString runs a no-argument method and reads its answer as text, falling back to to_s when the method answers something that is not a String.
func (vm *VM) callString(ref int32, name string) (string, error) {
	if err := vm.in.ArgsReset(); err != nil {
		return "", err
	}
	raw, err := vm.in.Call(ref, name, false)
	if err != nil {
		return "", vm.wrap(err)
	}
	if raw.Kind == mrubyvm.KindString || raw.Kind == mrubyvm.KindSymbol {
		return raw.Str, nil
	}
	return vm.in.ResultToS()
}

// identitySource is a Proc that hands its argument straight back: it is how a value crosses into the interpreter and comes back out in the other form.
const identitySource = "->(x) { x }"

// materialize gives an immediate a reference of its own, which the caller owns; it is how an immediate becomes a receiver.
func (vm *VM) materialize(v *Value) (int32, error) {
	proc, err := vm.constant(&vm.identity, identitySource)
	if err != nil {
		return 0, err
	}
	a, err := v.arg(vm)
	if err != nil {
		return 0, err
	}
	if err := vm.in.ArgsReset(); err != nil {
		return 0, err
	}
	if err := a.push(vm.in); err != nil {
		return 0, err
	}
	if _, err := vm.in.Yield(proc, false); err != nil {
		return 0, vm.wrap(err)
	}
	ref, err := vm.in.ResultRef()
	if err != nil {
		return 0, err
	}
	if ref == 0 {
		return 0, errors.New("mruby: the interpreter would not hold on to the value")
	}
	return ref, nil
}

// constant evaluates src once and keeps the reference for the life of the VM.
func (vm *VM) constant(cached *int32, src string) (int32, error) {
	if *cached != 0 {
		return *cached, nil
	}
	raw, err := vm.in.Eval(src, true)
	if err != nil {
		return 0, vm.wrap(err)
	}
	if raw.Ref == 0 {
		return 0, fmt.Errorf("mruby: the interpreter would not hold on to %s", src)
	}
	*cached = raw.Ref
	return raw.Ref, nil
}

// objectID is Ruby's object identity, which is what tells an already visited container from an equal one.
func (vm *VM) objectID(ref int32) (int64, error) {
	if err := vm.in.ArgsReset(); err != nil {
		return 0, err
	}
	raw, err := vm.in.Call(ref, "object_id", false)
	if err != nil {
		return 0, vm.wrap(err)
	}
	if raw.Kind != mrubyvm.KindInt {
		return 0, errors.New("mruby: object_id did not answer an Integer")
	}
	return raw.Int, nil
}

func referenced(k mrubyvm.Kind) bool {
	return k == mrubyvm.KindArray || k == mrubyvm.KindHash || k == mrubyvm.KindOther
}
