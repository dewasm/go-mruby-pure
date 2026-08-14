package mruby

import (
	"bytes"
	"errors"
	"math"
	"strings"
	"testing"
)

func newVM(t testing.TB, opts ...Option) *VM {
	t.Helper()
	vm, err := New(opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// This build of mruby has no Kernel#Integer and friends, so the tests send a value through a method of their own.
	if _, err := vm.Eval("def identity(x); x; end"); err != nil {
		t.Fatalf("defining identity: %v", err)
	}
	return vm
}

func identity(t *testing.T, vm *VM, x any) *Value {
	t.Helper()
	v, err := vm.Call(nil, "identity", x)
	if err != nil {
		t.Fatalf("identity(%#v): %v", x, err)
	}
	return v
}

func mustEval(t *testing.T, vm *VM, src string) *Value {
	t.Helper()
	v, err := vm.Eval(src)
	if err != nil {
		t.Fatalf("Eval(%q): %v", src, err)
	}
	return v
}

func TestEvalImmediates(t *testing.T) {
	vm := newVM(t)
	for _, c := range []struct {
		src  string
		want Type
	}{
		{"nil", TypeNil},
		{"true", TypeBool},
		{"false", TypeBool},
		{"42", TypeInteger},
		{"1.5", TypeFloat},
		{"'text'", TypeString},
		{":sym", TypeSymbol},
		{"[1]", TypeArray},
		{"{a: 1}", TypeHash},
		{"Object.new", TypeObject},
		{"->(x) { x }", TypeObject},
	} {
		if got := mustEval(t, vm, c.src).Type(); got != c.want {
			t.Errorf("Eval(%q).Type() = %v, want %v", c.src, got, c.want)
		}
	}
}

func TestIntegerRoundTrip(t *testing.T) {
	vm := newVM(t)
	for _, n := range []int64{0, 1, -1, math.MaxInt64, math.MinInt64, math.MaxInt32 + 1} {
		got, err := identity(t, vm, n).Int()
		if err != nil {
			t.Fatalf("Int of %d: %v", n, err)
		}
		if got != n {
			t.Errorf("%d came back as %d", n, got)
		}
	}
}

func TestFloatRoundTrip(t *testing.T) {
	vm := newVM(t)
	for _, f := range []float64{0, math.Copysign(0, -1), 1.5, -1.5, math.MaxFloat64, math.SmallestNonzeroFloat64, math.Inf(1), math.Inf(-1)} {
		got, err := identity(t, vm, f).Float()
		if err != nil {
			t.Fatal(err)
		}
		if got != f {
			t.Errorf("%v came back as %v", f, got)
		}
		if math.Signbit(f) != math.Signbit(got) {
			t.Errorf("the sign of %v was lost: %v", f, got)
		}
	}

	got, err := identity(t, vm, math.NaN()).Float()
	if err != nil {
		t.Fatal(err)
	}
	if !math.IsNaN(got) {
		t.Errorf("NaN came back as %v", got)
	}
}

func TestStringAndSymbolRoundTrip(t *testing.T) {
	vm := newVM(t)
	for _, s := range []string{"", "ascii", "日本語 と emoji 🍣", "a\x00b", "\xff\xfe raw bytes", strings.Repeat("long", 1000)} {
		got, err := identity(t, vm, s).Text()
		if err != nil {
			t.Fatal(err)
		}
		if got != s {
			t.Errorf("%q came back as %q", s, got)
		}
	}

	sym := identity(t, vm, Symbol("a symbol"))
	if sym.Type() != TypeSymbol {
		t.Fatalf("a Symbol crossed as %v", sym.Type())
	}
	if got, _ := sym.Text(); got != "a symbol" {
		t.Errorf("the Symbol came back as %q", got)
	}
	back := mustEval(t, vm, ":round_trip")
	if back.Type() != TypeSymbol {
		t.Fatalf("a Symbol came back as %v", back.Type())
	}
	x, err := back.Export()
	if err != nil {
		t.Fatal(err)
	}
	if x != Symbol("round_trip") {
		t.Errorf("Export of a Symbol = %#v", x)
	}
}

func TestBytesCrossAsString(t *testing.T) {
	vm := newVM(t)
	v := identity(t, vm, []byte{0, 1, 255})
	got, err := v.Text()
	if err != nil {
		t.Fatal(err)
	}
	if got != "\x00\x01\xff" {
		t.Errorf("[]byte came back as %q", got)
	}
	n, err := v.Call("bytesize")
	if err != nil {
		t.Fatal(err)
	}
	if size, _ := n.Int(); size != 3 {
		t.Errorf("bytesize = %d, want 3", size)
	}
}

func TestCallOnValues(t *testing.T) {
	vm := newVM(t)

	sum := identity(t, vm, 40)
	// An immediate receiver crosses back into the interpreter for the call.
	got, err := sum.Call("+", 2)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := got.Int(); n != 42 {
		t.Errorf("40 + 2 = %d", n)
	}

	ary := mustEval(t, vm, "[3, 1, 2]")
	sorted, err := ary.Call("sort")
	if err != nil {
		t.Fatal(err)
	}
	x, err := sorted.Export()
	if err != nil {
		t.Fatal(err)
	}
	want := []any{int64(1), int64(2), int64(3)}
	if !equalAny(x, want) {
		t.Errorf("sort = %#v, want %#v", x, want)
	}
}

func TestCallCompositeArguments(t *testing.T) {
	vm := newVM(t)

	v := identity(t, vm, []any{1, "two", Symbol("three"), []any{4, nil}, true})
	x, err := v.Export()
	if err != nil {
		t.Fatal(err)
	}
	want := []any{int64(1), "two", Symbol("three"), []any{int64(4), nil}, true}
	if !equalAny(x, want) {
		t.Errorf("the Array came back as %#v, want %#v", x, want)
	}

	h := identity(t, vm, map[any]any{"a": 1, Symbol("b"): []any{2}, 3: 4.5})
	x, err = h.Export()
	if err != nil {
		t.Fatal(err)
	}
	want2 := map[any]any{"a": int64(1), Symbol("b"): []any{int64(2)}, int64(3): 4.5}
	if !equalAny(x, want2) {
		t.Errorf("the Hash came back as %#v, want %#v", x, want2)
	}

	// A map[string]any crosses with String keys.
	x, err = identity(t, vm, map[string]any{"k": "v"}).Export()
	if err != nil {
		t.Fatal(err)
	}
	if !equalAny(x, map[any]any{"k": "v"}) {
		t.Errorf("a map[string]any came back as %#v", x)
	}
}

func TestUnsignedArgumentOverflow(t *testing.T) {
	vm := newVM(t)

	if n, _ := identity(t, vm, uint64(math.MaxInt64)).Int(); n != math.MaxInt64 {
		t.Errorf("uint64(MaxInt64) = %d", n)
	}
	if _, err := vm.Call(nil, "identity", uint64(math.MaxInt64)+1); err == nil {
		t.Fatal("a uint64 past MaxInt64 crossed into Ruby")
	} else if !strings.Contains(err.Error(), "9223372036854775808") {
		t.Errorf("the error does not name the number: %v", err)
	}
}

func TestUnsupportedArgumentType(t *testing.T) {
	vm := newVM(t)
	_, err := vm.Call(nil, "identity", struct{ A int }{1})
	if err == nil {
		t.Fatal("a struct crossed into Ruby")
	}
	if !strings.Contains(err.Error(), "struct { A int }") {
		t.Errorf("the error does not name the Go type: %v", err)
	}
	if _, err := vm.Call(nil, "identity", []string{"no"}); err == nil {
		t.Error("a []string crossed into Ruby")
	}
}

func TestRubyErrorFromEval(t *testing.T) {
	vm := newVM(t)
	_, err := vm.Eval("def boom; raise ArgumentError, 'bad thing'; end; boom")

	var re *RubyError
	if !errors.As(err, &re) {
		t.Fatalf("Eval error = %v (%T), want *RubyError", err, err)
	}
	if re.Class != "ArgumentError" || re.Message != "bad thing" {
		t.Errorf("error = %s: %s", re.Class, re.Message)
	}
	if len(re.Backtrace) == 0 {
		t.Fatal("the error carries no backtrace")
	}
	if !strings.Contains(strings.Join(re.Backtrace, "\n"), "boom") {
		t.Errorf("backtrace = %q, want it to name the raising method", re.Backtrace)
	}
	if got, want := re.Error(), "ArgumentError: bad thing"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestRubyErrorFromCall(t *testing.T) {
	vm := newVM(t)
	if _, err := vm.Eval("def raiser; raise TypeError, 'from a call'; end"); err != nil {
		t.Fatal(err)
	}
	_, err := vm.Call(nil, "raiser")

	var re *RubyError
	if !errors.As(err, &re) {
		t.Fatalf("Call error = %v (%T), want *RubyError", err, err)
	}
	if re.Class != "TypeError" || re.Message != "from a call" {
		t.Errorf("error = %s: %s", re.Class, re.Message)
	}

	// A method that is not there is an ordinary Ruby exception too.
	_, err = vm.Call(nil, "no_such_method")
	if !errors.As(err, &re) || re.Class != "NoMethodError" {
		t.Errorf("calling a method that is not there gave %v", err)
	}
}

func TestSyntaxError(t *testing.T) {
	vm := newVM(t)
	_, err := vm.Eval("1 +")

	var re *RubyError
	if !errors.As(err, &re) {
		t.Fatalf("Eval error = %v (%T), want *RubyError", err, err)
	}
	if re.Class != "SyntaxError" {
		t.Errorf("Class = %q, want SyntaxError", re.Class)
	}
	if v := mustEval(t, vm, "1 + 1"); v.String() != "2" {
		t.Error("the VM did not survive a syntax error")
	}
}

func TestExitIsAnError(t *testing.T) {
	vm := newVM(t)
	_, err := vm.Eval("exit! 3")

	var ee *ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("Eval(exit!) = %v (%T), want *ExitError", err, err)
	}
	if ee.Code != 3 {
		t.Errorf("exit code = %d, want 3", ee.Code)
	}
}

func TestEvalKeepsLocalVariables(t *testing.T) {
	vm := newVM(t)
	mustEval(t, vm, "x = 7")
	if v := mustEval(t, vm, "x * 6"); v.String() != "42" {
		t.Errorf("a local variable did not survive into the next Eval: %s", v)
	}
}

func TestStdoutAndStderrCapture(t *testing.T) {
	var out, errOut bytes.Buffer
	vm := newVM(t, WithStdout(&out), WithStderr(&errOut))

	mustEval(t, vm, "print 'no newline'")
	mustEval(t, vm, "puts 'and a line'")
	if got, want := out.String(), "no newlineand a line\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
	if errOut.Len() != 0 {
		t.Errorf("stderr = %q, want nothing", errOut.String())
	}

	other := newVM(t)
	mustEval(t, other, "puts 'elsewhere'")
	if out.String() != "no newlineand a line\n" {
		t.Errorf("another VM wrote to this one's stdout: %q", out.String())
	}
}

func TestVMsAreIndependent(t *testing.T) {
	a, b := newVM(t), newVM(t)
	mustEval(t, a, "$state = 'a'")
	mustEval(t, b, "$state = 'b'")
	if got := mustEval(t, a, "$state").String(); got != "a" {
		t.Errorf("VM a sees %q", got)
	}
	if got := mustEval(t, b, "$state").String(); got != "b" {
		t.Errorf("VM b sees %q", got)
	}

	// A value belongs to the VM that made it.
	v := mustEval(t, a, "[1]")
	if _, err := b.Call(nil, "identity", v); err == nil {
		t.Error("a value crossed into another VM")
	}
}

// equalAny compares exported trees, where the only composites are []any and map[any]any.
func equalAny(a, b any) bool {
	switch a := a.(type) {
	case []any:
		b, ok := b.([]any)
		if !ok || len(a) != len(b) {
			return false
		}
		for i := range a {
			if !equalAny(a[i], b[i]) {
				return false
			}
		}
		return true
	case map[any]any:
		b, ok := b.(map[any]any)
		if !ok || len(a) != len(b) {
			return false
		}
		for k, v := range a {
			other, ok := b[k]
			if !ok || !equalAny(v, other) {
				return false
			}
		}
		return true
	}
	return a == b
}

func TestTheVMsOwnHelpersLeaveNoTrace(t *testing.T) {
	vm := newVM(t)

	// Building an Array argument and calling on an immediate receiver both make the VM evaluate helpers of its own.
	if _, err := vm.Call(nil, "identity", []any{1, map[any]any{"k": "v"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := mustEval(t, vm, "7").Call("+", 1); err != nil {
		t.Fatal(err)
	}
	if got := mustEval(t, vm, "'text'").Inspect(); got != `"text"` {
		t.Fatalf("Inspect of a String = %s", got)
	}
	// The helpers are evaluated in the same compiler context as the caller's code, so a local variable of theirs would be one of the caller's too.
	if got := mustEval(t, vm, "local_variables.inspect").String(); got != "[]" {
		t.Errorf("the eval context carries local variables: %s", got)
	}
}
