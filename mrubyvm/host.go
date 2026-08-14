// Package mrubyvm is the low-level Go side of the mruby wasm ABI.
//
// mruby_gen.go, the dewasm translation of tools/cache/mruby_shim.wasm, declares this same package; the instance's linear memory, its bundled WASI and the runtime's trap/exit types are unexported, so glue reaching them has to live here.
// Everything below is a thin wrapper over one `dm_*` guest export: no value caching, no type coercion, no object lifetime management beyond the ref queue.
package mrubyvm

import (
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// Kind classifies a Ruby value the way the guest's dm_result_kind does.
type Kind int32

const (
	KindNil Kind = iota
	KindFalse
	KindTrue
	KindInt
	KindFloat
	KindString
	KindSymbol
	KindArray
	KindHash
	KindOther
)

// KindInvalid is what the guest answers for a ref it does not know and for a host-argument index out of range.
const KindInvalid Kind = -1

func (k Kind) String() string {
	switch k {
	case KindNil:
		return "nil"
	case KindFalse:
		return "false"
	case KindTrue:
		return "true"
	case KindInt:
		return "Integer"
	case KindFloat:
		return "Float"
	case KindString:
		return "String"
	case KindSymbol:
		return "Symbol"
	case KindArray:
		return "Array"
	case KindHash:
		return "Hash"
	case KindOther:
		return "object"
	}
	return "invalid"
}

// Value is one readout of the guest's value capture.
// Int, Float and Str each carry a value only for the matching Kind; Ref is non-zero only when the call was asked for one, and the caller then owns it.
type Value struct {
	Kind  Kind
	Int   int64
	Float float64
	Str   string
	Ref   int32
}

// RubyError is a Ruby exception that reached the boundary.
type RubyError struct {
	Class     string
	Message   string
	Backtrace string
}

func (e *RubyError) Error() string {
	return e.Class + ": " + e.Message
}

// ABIError is the guest refusing a call it cannot make sense of: no dm_init, a released ref, a ref of the wrong kind.
type ABIError struct {
	Message string
}

func (e *ABIError) Error() string {
	return "mrubyvm: " + e.Message
}

// ExitError is a WASI proc_exit from inside the guest, which ends the usefulness of the instance: mruby's Kernel#exit! reaches it.
type ExitError struct {
	Code int
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("mrubyvm: the guest exited with status %d", e.Code)
}

// TrapError is a wasm trap: a bug in the shim or a resource limit, never a Ruby-level condition.
type TrapError struct {
	Message string
}

func (e *TrapError) Error() string {
	return "mrubyvm: wasm trap: " + e.Message
}

// Instance is one mruby VM: one wasm instance, one mrb_state.
//
// Calls serialize on a mutex, so an Instance may be handed between goroutines.
// The one exception is reentrancy: a host callback runs on the goroutine that already holds the lock, and the calls it makes back into the guest deliberately skip locking, so while any host callback is in flight no other goroutine may call this Instance.
type Instance struct {
	mod *Mrubyvm
	fn  dmFuncs

	mu        sync.Mutex
	hostDepth atomic.Int32

	stdout   io.Writer
	stderr   io.Writer
	hostCall func(*Instance) int32

	releaseMu sync.Mutex
	release   []int32

	// One entry per host callback in flight; only the goroutine running them touches it.
	fnIDs []int32
}

// NewInstance builds a VM writing guest stdout and stderr to the given writers (nil discards) and routing Ruby calls to Go-defined methods through hostCall, which returns 0 for a normal return and 1 to raise what it passed to HostRaise.
func NewInstance(stdout, stderr io.Writer, hostCall func(*Instance) int32) (_ *Instance, err error) {
	in := &Instance{stdout: stdout, stderr: stderr, hostCall: hostCall}
	if in.stdout == nil {
		in.stdout = io.Discard
	}
	if in.stderr == nil {
		in.stderr = io.Discard
	}
	defer in.guard(&err)

	imports := Imports{
		"host":                   map[string]any{"call": in.hostEntry},
		"wasi_snapshot_preview1": map[string]any{"fd_write": in.fdWrite},
	}
	in.mod = NewMrubyvm(imports, nil, nil, nil)
	if in.fn, err = bindFuncs(in.mod); err != nil {
		return nil, err
	}
	// A reactor runs its C constructors in _initialize, so libc is unusable before it.
	in.fn.initialize()
	if rc := in.fn.vmInit(); rc != 0 {
		return nil, in.status(rc)
	}
	return in, nil
}

// --- export binding ------------------------------------------------------

type dmFuncs struct {
	initialize func()

	vmInit func() uint32
	alloc  func(uint32) uint32
	free   func(uint32)

	eval      func(uint32, uint32) uint32
	call      func(uint32, uint32, uint32) uint32
	callBlock func(uint32, uint32, uint32, uint32) uint32
	yield     func(uint32) uint32

	resultKind   func() uint32
	resultInt    func() uint64
	resultFloat  func() float64
	resultStr    func() uint32
	resultStrLen func() uint32
	resultToS    func() uint32
	resultRef    func() uint32

	errorClass        func() uint32
	errorClassLen     func() uint32
	errorMessage      func() uint32
	errorMessageLen   func() uint32
	errorBacktrace    func() uint32
	errorBacktraceLen func() uint32
	errorRef          func() uint32

	refRelease  func(uint32)
	kind        func(uint32) uint32
	registryLen func() uint32

	argsReset     func()
	argsPushNil   func()
	argsPushBool  func(uint32)
	argsPushInt   func(uint64)
	argsPushFloat func(float64)
	argsPushStr   func(uint32, uint32)
	argsPushSym   func(uint32, uint32)
	argsPushRef   func(uint32)

	aryLen   func(uint32) uint64
	aryGet   func(uint32, uint64) uint32
	hashKeys func(uint32) uint32
	hashGet  func(uint32, uint32) uint32

	defineClass          func(uint32, uint32, uint32) uint32
	defineMethod         func(uint32, uint32, uint32, uint32) uint32
	defineModuleFunction func(uint32, uint32, uint32, uint32) uint32

	hostargsCount  func() uint32
	hostargsKind   func(uint32) uint32
	hostargsInt    func(uint32) uint64
	hostargsFloat  func(uint32) float64
	hostargsStr    func(uint32) uint32
	hostargsStrLen func(uint32) uint32
	hostargsRef    func(uint32) uint32
	hostargsSelf   func() uint32
	hostargsBlock  func() uint32
	hostRaise      func(uint32, uint32, uint32, uint32)
}

func bindTo[T any](m *Mrubyvm, name string, dst *T, err *error) {
	v, ok := m.Exports[name]
	if !ok {
		if *err == nil {
			*err = fmt.Errorf("mrubyvm: the generated module exports no %q", name)
		}
		return
	}
	f, ok := v.(T)
	if !ok {
		if *err == nil {
			*err = fmt.Errorf("mrubyvm: export %q is %T, want %T", name, v, *dst)
		}
		return
	}
	*dst = f
}

func bindFuncs(m *Mrubyvm) (dmFuncs, error) {
	var f dmFuncs
	var err error
	bindTo(m, "_initialize", &f.initialize, &err)
	bindTo(m, "dm_init", &f.vmInit, &err)
	bindTo(m, "dm_alloc", &f.alloc, &err)
	bindTo(m, "dm_free", &f.free, &err)
	bindTo(m, "dm_eval", &f.eval, &err)
	bindTo(m, "dm_call", &f.call, &err)
	bindTo(m, "dm_call_block", &f.callBlock, &err)
	bindTo(m, "dm_yield", &f.yield, &err)
	bindTo(m, "dm_result_kind", &f.resultKind, &err)
	bindTo(m, "dm_result_int", &f.resultInt, &err)
	bindTo(m, "dm_result_float", &f.resultFloat, &err)
	bindTo(m, "dm_result_str", &f.resultStr, &err)
	bindTo(m, "dm_result_str_len", &f.resultStrLen, &err)
	bindTo(m, "dm_result_to_s", &f.resultToS, &err)
	bindTo(m, "dm_result_ref", &f.resultRef, &err)
	bindTo(m, "dm_error_class", &f.errorClass, &err)
	bindTo(m, "dm_error_class_len", &f.errorClassLen, &err)
	bindTo(m, "dm_error_message", &f.errorMessage, &err)
	bindTo(m, "dm_error_message_len", &f.errorMessageLen, &err)
	bindTo(m, "dm_error_backtrace", &f.errorBacktrace, &err)
	bindTo(m, "dm_error_backtrace_len", &f.errorBacktraceLen, &err)
	bindTo(m, "dm_error_ref", &f.errorRef, &err)
	bindTo(m, "dm_ref_release", &f.refRelease, &err)
	bindTo(m, "dm_kind", &f.kind, &err)
	bindTo(m, "dm_registry_len", &f.registryLen, &err)
	bindTo(m, "dm_args_reset", &f.argsReset, &err)
	bindTo(m, "dm_args_push_nil", &f.argsPushNil, &err)
	bindTo(m, "dm_args_push_bool", &f.argsPushBool, &err)
	bindTo(m, "dm_args_push_int", &f.argsPushInt, &err)
	bindTo(m, "dm_args_push_float", &f.argsPushFloat, &err)
	bindTo(m, "dm_args_push_str", &f.argsPushStr, &err)
	bindTo(m, "dm_args_push_sym", &f.argsPushSym, &err)
	bindTo(m, "dm_args_push_ref", &f.argsPushRef, &err)
	bindTo(m, "dm_ary_len", &f.aryLen, &err)
	bindTo(m, "dm_ary_get", &f.aryGet, &err)
	bindTo(m, "dm_hash_keys", &f.hashKeys, &err)
	bindTo(m, "dm_hash_get", &f.hashGet, &err)
	bindTo(m, "dm_define_class", &f.defineClass, &err)
	bindTo(m, "dm_define_method", &f.defineMethod, &err)
	bindTo(m, "dm_define_module_function", &f.defineModuleFunction, &err)
	bindTo(m, "dm_hostargs_count", &f.hostargsCount, &err)
	bindTo(m, "dm_hostargs_kind", &f.hostargsKind, &err)
	bindTo(m, "dm_hostargs_int", &f.hostargsInt, &err)
	bindTo(m, "dm_hostargs_float", &f.hostargsFloat, &err)
	bindTo(m, "dm_hostargs_str", &f.hostargsStr, &err)
	bindTo(m, "dm_hostargs_str_len", &f.hostargsStrLen, &err)
	bindTo(m, "dm_hostargs_ref", &f.hostargsRef, &err)
	bindTo(m, "dm_hostargs_self", &f.hostargsSelf, &err)
	bindTo(m, "dm_hostargs_block", &f.hostargsBlock, &err)
	bindTo(m, "dm_host_raise", &f.hostRaise, &err)
	return f, err
}

// --- entry discipline ----------------------------------------------------

// enter serializes guest calls and returns the matching leave.
// A call made from inside a host callback runs on the goroutine that already holds the lock, so it must not take it again.
func (in *Instance) enter() func() {
	if in.hostDepth.Load() > 0 {
		return func() {}
	}
	in.mu.Lock()
	in.drainReleases()
	return in.mu.Unlock
}

// guard turns a panic the generated code raised into an error.
// Any other panic keeps unwinding: it is a bug in this package or in the host, not a guest condition.
func (in *Instance) guard(err *error) {
	r := recover()
	if r == nil {
		return
	}
	if e := guestError(r); e != nil {
		*err = e
		return
	}
	panic(r)
}

func guestError(r any) error {
	switch e := r.(type) {
	case *rtExit:
		return &ExitError{Code: e.code}
	case *rtTrap:
		return &TrapError{Message: e.msg}
	case *rtLinkError:
		return fmt.Errorf("mrubyvm: %s", e.msg)
	}
	return nil
}

// GuestError is the error behind a panic the generated code raised, nil for a panic from anywhere else.
// A recover placed between a host callback and hostEntry has to re-panic what this reports: those panics end the instance and no Ruby raise can stand in for them.
func GuestError(r any) error {
	return guestError(r)
}

// ReleaseRef hands a ref back to the guest registry.
// It takes no guest call and no VM lock, so it is safe from a finalizer or another goroutine; the queue is drained at the next call that takes the lock.
func (in *Instance) ReleaseRef(ref int32) {
	if ref == 0 {
		return
	}
	in.releaseMu.Lock()
	in.release = append(in.release, ref)
	in.releaseMu.Unlock()
}

func (in *Instance) drainReleases() {
	in.releaseMu.Lock()
	queued := in.release
	in.release = nil
	in.releaseMu.Unlock()
	for _, ref := range queued {
		in.fn.refRelease(uint32(ref))
	}
}

// --- guest memory --------------------------------------------------------

// guestStr is a byte range the host owns inside guest memory.
type guestStr struct {
	ptr uint32
	len uint32
}

func (in *Instance) push(s string) (guestStr, error) {
	b := []byte(s)
	ptr := in.fn.alloc(uint32(len(b)))
	if ptr == 0 {
		return guestStr{}, &ABIError{Message: "the guest allocator returned no memory"}
	}
	if len(b) > 0 {
		in.mod.memory.init(uint64(ptr), b, 0, uint64(len(b)))
	}
	return guestStr{ptr: ptr, len: uint32(len(b))}, nil
}

func (in *Instance) drop(g guestStr) {
	in.fn.free(g.ptr)
}

// read copies bytes out of guest memory; the guest only pins them until its next call, so nothing here holds on to the slice.
func (in *Instance) read(ptr uint32, length uint32) string {
	if ptr == 0 || length == 0 || int32(length) < 0 {
		return ""
	}
	return string(in.mod.memory.read_string(uint64(ptr), uint64(length)))
}

// --- status and captures -------------------------------------------------

func (in *Instance) status(rc uint32) error {
	switch rc {
	case 0:
		return nil
	case 1:
		return in.rubyError()
	default:
		return &ABIError{Message: in.read(in.fn.errorMessage(), in.fn.errorMessageLen())}
	}
}

func (in *Instance) rubyError() *RubyError {
	return &RubyError{
		Class:     in.read(in.fn.errorClass(), in.fn.errorClassLen()),
		Message:   in.read(in.fn.errorMessage(), in.fn.errorMessageLen()),
		Backtrace: in.read(in.fn.errorBacktrace(), in.fn.errorBacktraceLen()),
	}
}

// ErrorRef registers the exception object of the last failed call and returns the ref, 0 when there is none.
func (in *Instance) ErrorRef() (ref int32, err error) {
	defer in.enter()()
	defer in.guard(&err)
	return int32(in.fn.errorRef()), nil
}

func (in *Instance) value(wantRef bool) Value {
	v := Value{Kind: Kind(int32(in.fn.resultKind()))}
	switch v.Kind {
	case KindInt:
		v.Int = int64(in.fn.resultInt())
	case KindFloat:
		v.Float = in.fn.resultFloat()
	case KindString, KindSymbol:
		v.Str = in.read(in.fn.resultStr(), in.fn.resultStrLen())
	}
	if wantRef {
		v.Ref = int32(in.fn.resultRef())
	}
	return v
}

// --- evaluation and calls ------------------------------------------------

// Eval compiles and runs src in the VM's persistent compiler context, so local variables defined by one Eval are visible to the next.
func (in *Instance) Eval(src string, wantRef bool) (v Value, err error) {
	defer in.enter()()
	defer in.guard(&err)

	g, err := in.push(src)
	if err != nil {
		return Value{}, err
	}
	defer in.drop(g)
	if err := in.status(in.fn.eval(g.ptr, g.len)); err != nil {
		return Value{}, err
	}
	return in.value(wantRef), nil
}

// Call invokes name on the ref's value, or on the top-level self when recvRef is 0, consuming whatever the Push* methods left in the argument scratch.
func (in *Instance) Call(recvRef int32, name string, wantRef bool) (v Value, err error) {
	defer in.enter()()
	defer in.guard(&err)

	g, err := in.push(name)
	if err != nil {
		return Value{}, err
	}
	defer in.drop(g)
	if err := in.status(in.fn.call(uint32(recvRef), g.ptr, g.len)); err != nil {
		return Value{}, err
	}
	return in.value(wantRef), nil
}

// CallBlock is Call with a Proc ref passed as the method's block; blockRef 0 passes none.
func (in *Instance) CallBlock(recvRef int32, name string, blockRef int32, wantRef bool) (v Value, err error) {
	defer in.enter()()
	defer in.guard(&err)

	g, err := in.push(name)
	if err != nil {
		return Value{}, err
	}
	defer in.drop(g)
	if err := in.status(in.fn.callBlock(uint32(recvRef), g.ptr, g.len, uint32(blockRef))); err != nil {
		return Value{}, err
	}
	return in.value(wantRef), nil
}

// Yield calls a Proc with the argument scratch as its arguments.
func (in *Instance) Yield(procRef int32, wantRef bool) (v Value, err error) {
	defer in.enter()()
	defer in.guard(&err)

	if err := in.status(in.fn.yield(uint32(procRef))); err != nil {
		return Value{}, err
	}
	return in.value(wantRef), nil
}

// ResultRef registers the last captured value and returns the ref, which the caller owns.
// It is the deferred form of the wantRef argument, for a caller that reads the kind first and only wants a ref for some of them.
func (in *Instance) ResultRef() (ref int32, err error) {
	defer in.enter()()
	defer in.guard(&err)
	return int32(in.fn.resultRef()), nil
}

// ResultToS replaces the string readout of the last captured value with its to_s, which is the only way to get one for a kind that is neither String nor Symbol.
func (in *Instance) ResultToS() (s string, err error) {
	defer in.enter()()
	defer in.guard(&err)

	if err := in.status(in.fn.resultToS()); err != nil {
		return "", err
	}
	return in.read(in.fn.resultStr(), in.fn.resultStrLen()), nil
}

// --- argument scratch ----------------------------------------------------

// ArgsReset empties the argument scratch, which is where the next Call/CallBlock/Yield takes its arguments and, inside a host callback, where the return value goes.
func (in *Instance) ArgsReset() (err error) {
	defer in.enter()()
	defer in.guard(&err)
	in.fn.argsReset()
	return nil
}

func (in *Instance) PushNil() (err error) {
	defer in.enter()()
	defer in.guard(&err)
	in.fn.argsPushNil()
	return nil
}

func (in *Instance) PushBool(b bool) (err error) {
	defer in.enter()()
	defer in.guard(&err)
	var v uint32
	if b {
		v = 1
	}
	in.fn.argsPushBool(v)
	return nil
}

func (in *Instance) PushInt(i int64) (err error) {
	defer in.enter()()
	defer in.guard(&err)
	in.fn.argsPushInt(uint64(i))
	return nil
}

func (in *Instance) PushFloat(f float64) (err error) {
	defer in.enter()()
	defer in.guard(&err)
	in.fn.argsPushFloat(f)
	return nil
}

func (in *Instance) PushString(s string) (err error) {
	defer in.enter()()
	defer in.guard(&err)
	g, err := in.push(s)
	if err != nil {
		return err
	}
	defer in.drop(g)
	in.fn.argsPushStr(g.ptr, g.len)
	return nil
}

func (in *Instance) PushSymbol(s string) (err error) {
	defer in.enter()()
	defer in.guard(&err)
	g, err := in.push(s)
	if err != nil {
		return err
	}
	defer in.drop(g)
	in.fn.argsPushSym(g.ptr, g.len)
	return nil
}

func (in *Instance) PushRef(ref int32) (err error) {
	defer in.enter()()
	defer in.guard(&err)
	in.fn.argsPushRef(uint32(ref))
	return nil
}

// --- refs and structure --------------------------------------------------

// RefKind classifies a held ref, KindInvalid when the guest does not know it.
func (in *Instance) RefKind(ref int32) (k Kind, err error) {
	defer in.enter()()
	defer in.guard(&err)
	return Kind(int32(in.fn.kind(uint32(ref)))), nil
}

// RegistryLen is the registry's high-water mark: released slots are reused, so this is what shows whether refs leak.
func (in *Instance) RegistryLen() (n int32, err error) {
	defer in.enter()()
	defer in.guard(&err)
	return int32(in.fn.registryLen()), nil
}

// ArrayLen is the length of the Array behind ref, -1 when it is not one.
func (in *Instance) ArrayLen(ref int32) (n int64, err error) {
	defer in.enter()()
	defer in.guard(&err)
	return int64(in.fn.aryLen(uint32(ref))), nil
}

// ArrayGet reads one element; an index out of range reads as nil, matching Ruby.
func (in *Instance) ArrayGet(ref int32, i int64, wantRef bool) (v Value, err error) {
	defer in.enter()()
	defer in.guard(&err)

	if err := in.status(in.fn.aryGet(uint32(ref), uint64(i))); err != nil {
		return Value{}, err
	}
	return in.value(wantRef), nil
}

// HashKeys returns a ref to a new Array of the Hash's keys, which the caller owns.
func (in *Instance) HashKeys(ref int32) (keys int32, err error) {
	defer in.enter()()
	defer in.guard(&err)

	keys = int32(in.fn.hashKeys(uint32(ref)))
	if keys == 0 {
		return 0, in.status(2)
	}
	return keys, nil
}

// HashGet looks a key up by ref, which is what lets a key of any kind cross the boundary.
func (in *Instance) HashGet(ref, keyRef int32, wantRef bool) (v Value, err error) {
	defer in.enter()()
	defer in.guard(&err)

	if err := in.status(in.fn.hashGet(uint32(ref), uint32(keyRef))); err != nil {
		return Value{}, err
	}
	return in.value(wantRef), nil
}

// --- definitions ---------------------------------------------------------

// DefineClass defines name under Object with the class behind superRef, or Object when superRef is 0, and returns a ref to it.
func (in *Instance) DefineClass(name string, superRef int32) (ref int32, err error) {
	defer in.enter()()
	defer in.guard(&err)

	g, err := in.push(name)
	if err != nil {
		return 0, err
	}
	defer in.drop(g)
	if err := in.status(in.fn.defineClass(g.ptr, g.len, uint32(superRef))); err != nil {
		return 0, err
	}
	return int32(in.fn.resultRef()), nil
}

// DefineMethod makes name on the class or module behind targetRef, or on Kernel when targetRef is 0, call back into hostCall with fnID.
func (in *Instance) DefineMethod(targetRef int32, name string, fnID int32) (err error) {
	defer in.enter()()
	defer in.guard(&err)

	g, err := in.push(name)
	if err != nil {
		return err
	}
	defer in.drop(g)
	return in.status(in.fn.defineMethod(uint32(targetRef), g.ptr, g.len, uint32(fnID)))
}

// DefineModuleFunction is DefineMethod plus the same method on the target's singleton class.
func (in *Instance) DefineModuleFunction(targetRef int32, name string, fnID int32) (err error) {
	defer in.enter()()
	defer in.guard(&err)

	g, err := in.push(name)
	if err != nil {
		return err
	}
	defer in.drop(g)
	return in.status(in.fn.defineModuleFunction(uint32(targetRef), g.ptr, g.len, uint32(fnID)))
}

// --- host callbacks ------------------------------------------------------

// hostEntry is the guest's `host.call` import: it runs the embedder's callback while the guest is suspended inside a Ruby method.
// A panic from the callback becomes a Ruby exception rather than unwinding through the guest's frames, which would abandon the shim's frame stacks.
func (in *Instance) hostEntry(fnID uint32) (rc uint32) {
	in.hostDepth.Add(1)
	in.fnIDs = append(in.fnIDs, int32(fnID))
	defer func() {
		in.fnIDs = in.fnIDs[:len(in.fnIDs)-1]
		in.hostDepth.Add(-1)
	}()
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		if guestError(r) != nil {
			panic(r)
		}
		in.HostRaise("RuntimeError", fmt.Sprint(r))
		rc = 1
	}()

	if in.hostCall == nil {
		in.HostRaise("RuntimeError", "mrubyvm: no host callback is registered")
		return 1
	}
	return uint32(in.hostCall(in))
}

// HostFnID is the id DefineMethod registered for the method now calling back, valid only inside a host callback.
// The callback signature carries no id, so this is where the dispatch key comes from; nested callbacks read their own.
func (in *Instance) HostFnID() int32 {
	if len(in.fnIDs) == 0 {
		return 0
	}
	return in.fnIDs[len(in.fnIDs)-1]
}

// HostArgsCount is how many arguments the Ruby caller passed, valid only inside a host callback.
func (in *Instance) HostArgsCount() int32 {
	defer in.enter()()
	return int32(in.fn.hostargsCount())
}

// HostArgKind classifies argument i, KindInvalid when there is no such argument.
func (in *Instance) HostArgKind(i int32) Kind {
	defer in.enter()()
	return Kind(int32(in.fn.hostargsKind(uint32(i))))
}

func (in *Instance) HostArgInt(i int32) int64 {
	defer in.enter()()
	return int64(in.fn.hostargsInt(uint32(i)))
}

func (in *Instance) HostArgFloat(i int32) float64 {
	defer in.enter()()
	return in.fn.hostargsFloat(uint32(i))
}

// HostArgString is the bytes of a String or the name of a Symbol argument, empty for every other kind.
func (in *Instance) HostArgString(i int32) string {
	defer in.enter()()
	return in.read(in.fn.hostargsStr(uint32(i)), in.fn.hostargsStrLen(uint32(i)))
}

// HostArgRef registers argument i and returns the ref, which the caller owns.
func (in *Instance) HostArgRef(i int32) int32 {
	defer in.enter()()
	return int32(in.fn.hostargsRef(uint32(i)))
}

// HostSelfRef registers the receiver of the call and returns the ref, which the caller owns.
func (in *Instance) HostSelfRef() int32 {
	defer in.enter()()
	return int32(in.fn.hostargsSelf())
}

// HostBlockRef registers the block the Ruby caller passed and returns the ref, which the caller owns; 0 means no block was given.
func (in *Instance) HostBlockRef() int32 {
	defer in.enter()()
	return int32(in.fn.hostargsBlock())
}

// HostRaise records the exception a host callback returning 1 raises; class is resolved as a constant path under Object and falls back to RuntimeError.
func (in *Instance) HostRaise(class, message string) {
	defer in.enter()()

	c, err := in.push(class)
	if err != nil {
		return
	}
	defer in.drop(c)
	m, err := in.push(message)
	if err != nil {
		return
	}
	defer in.drop(m)
	in.fn.hostRaise(c.ptr, c.len, m.ptr, m.len)
}

// --- WASI ----------------------------------------------------------------

// fdWrite sends guest stdout and stderr to this instance's writers and leaves every other descriptor to the bundled WASI.
func (in *Instance) fdWrite(fd, iovsPtr, iovsLen, nwrittenPtr uint32) uint32 {
	var w io.Writer
	switch fd {
	case 1:
		w = in.stdout
	case 2:
		w = in.stderr
	default:
		return in.mod.wasiInstance().wasi_fd_write(fd, iovsPtr, iovsLen, nwrittenPtr)
	}

	mem := in.mod.memory
	written := uint32(0)
	for i := uint32(0); i < iovsLen; i++ {
		ptr := mem.i32_load(uint64(iovsPtr) + uint64(i)*8)
		length := mem.i32_load(uint64(iovsPtr) + uint64(i)*8 + 4)
		if length == 0 {
			continue
		}
		n, err := w.Write(mem.read_string(uint64(ptr), uint64(length)))
		written += uint32(n)
		if err != nil {
			mem.i32_store(uint64(nwrittenPtr), written)
			return wasiIo
		}
	}
	mem.i32_store(uint64(nwrittenPtr), written)
	return wasiOk
}
