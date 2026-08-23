// Package mruby runs Ruby code from Go.
//
// The mruby interpreter is compiled to WebAssembly and translated to Go source, so a VM is an ordinary Go value: no cgo, no shared library, nothing to close.
//
//	vm, err := mruby.New()
//	v, err := vm.Eval("1 + 2")
//	n, err := v.AsInt()
//
// # Conversions
//
// A closed list of Go types crosses into Ruby, as an argument, as a host function's result, as an element of either, or through [VM.ToValue]:
// nil, bool, every signed and unsigned integer type (a uint64 past [math.MaxInt64] is an error), float32 and float64,
// string, [Symbol], []byte as a String, []any as an Array, map[any]any and map[string]any as a Hash, and [Value].
// Anything else is an error naming the Go type.
//
// Coming back, [Value.GoValue] answers nil, bool, int64, float64, string, [Symbol], []any and map[any]any,
// and leaves a *[Value] where a Ruby value has no Go counterpart.
package mruby

import (
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/dewasm/go-mruby-pure/mrubyvm"
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

// EvalOption configures one evaluation.
type EvalOption func(*evalOptions)

type evalOptions struct {
	filename string
}

// WithFilename compiles the source under name, which is what `__FILE__` and a backtrace of it report; an evaluation without one reports `(eval)`.
func WithFilename(name string) EvalOption {
	return func(o *evalOptions) { o.filename = name }
}

// Eval compiles and runs src.
// One VM keeps one compiler context, so a local variable defined by one Eval is visible to the next.
func (vm *VM) Eval(src string, opts ...EvalOption) (*Value, error) {
	var o evalOptions
	for _, opt := range opts {
		opt(&o)
	}
	defer vm.enter()()

	raw, err := vm.eval(src, o.filename)
	if err != nil {
		return nil, vm.wrap(err)
	}
	return vm.capture(raw)
}

func (vm *VM) eval(src, filename string) (mrubyvm.Value, error) {
	if filename == "" {
		return vm.in.Eval(src, false)
	}
	return vm.in.EvalFile(src, filename, false)
}

// Call invokes the method name on recv, or on the top-level object when recv is nil.
// The arguments cross by the conversions in the package documentation.
func (vm *VM) Call(recv *Value, name string, args ...any) (*Value, error) {
	defer vm.enter()()
	return vm.call(recv, name, args)
}

// CallWithBlock is Call with block passed as the method's block, which is what a Ruby yield reaches; a nil block passes none.
func (vm *VM) CallWithBlock(recv *Value, name string, block *Value, args ...any) (*Value, error) {
	defer vm.enter()()
	return vm.callBlock(recv, name, block, args)
}

// FullGC runs a full garbage collection, which every Value the VM handed out survives.
func (vm *VM) FullGC() error {
	defer vm.enter()()

	if err := vm.in.FullGC(); err != nil {
		return vm.wrap(err)
	}
	return nil
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
	return vm.callBlock(recv, name, nil, args)
}

func (vm *VM) callBlock(recv *Value, name string, block *Value, args []any) (*Value, error) {
	b := builder{vm: vm}
	defer b.done()

	var recvRef, blockRef int32
	var err error
	if recv != nil {
		if recvRef, err = b.receiver(recv); err != nil {
			return nil, err
		}
	}
	if block != nil {
		if blockRef, err = b.receiver(block); err != nil {
			return nil, err
		}
	}
	if err := b.add(args...); err != nil {
		return nil, err
	}
	if err := b.flush(); err != nil {
		return nil, err
	}
	raw, err := vm.send(recvRef, name, blockRef)
	if err != nil {
		return nil, vm.wrap(err)
	}
	return vm.capture(raw)
}

// send is the call itself: a block ref of 0 is no block, which is the plain entry point.
func (vm *VM) send(recvRef int32, name string, blockRef int32) (mrubyvm.Value, error) {
	if blockRef == 0 {
		return vm.in.Call(recvRef, name, false)
	}
	return vm.in.CallBlock(recvRef, name, blockRef, false)
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

// materialize gives an immediate a reference of its own, which the caller owns; it is how an immediate becomes a receiver.
func (vm *VM) materialize(v *Value) (int32, error) {
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
	raw, err := vm.in.CaptureArg(true)
	if err != nil {
		return 0, vm.wrap(err)
	}
	if raw.Ref == 0 {
		return 0, errors.New("mruby: the interpreter would not hold on to the value")
	}
	return raw.Ref, nil
}

// objectID is the value's identity, which is what tells an already visited container from an equal one.
func (vm *VM) objectID(ref int32) (int64, error) {
	id, err := vm.in.ObjectID(ref)
	if err != nil {
		return 0, err
	}
	if id == 0 {
		return 0, errors.New("mruby: the interpreter does not know the value")
	}
	return id, nil
}

func referenced(k mrubyvm.Kind) bool {
	return k == mrubyvm.KindArray || k == mrubyvm.KindHash || k == mrubyvm.KindOther
}
