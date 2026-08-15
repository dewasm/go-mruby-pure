package mruby

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
)

func mustDefine(t *testing.T, vm *VM, name string, fn any) {
	t.Helper()
	if err := vm.Define(name, fn); err != nil {
		t.Fatalf("Define(%q): %v", name, err)
	}
}

func TestDefineArities(t *testing.T) {
	vm := newVM(t)
	mustDefine(t, vm, "go_none", func() int { return 7 })
	mustDefine(t, vm, "go_one", func(a int64) int64 { return a * 2 })
	mustDefine(t, vm, "go_three", func(a, b, c int) int { return a + b + c })
	mustDefine(t, vm, "go_rest", func(prefix string, rest ...any) string {
		return fmt.Sprintf("%s%d", prefix, len(rest))
	})

	for _, c := range []struct{ src, want string }{
		{"go_none", "7"},
		{"go_one(21)", "42"},
		{"go_three(1, 2, 3)", "6"},
		{"go_rest('n=')", "n=0"},
		{"go_rest('n=', 1, :two, [3])", "n=3"},
	} {
		if got := mustEval(t, vm, c.src).String(); got != c.want {
			t.Errorf("%s = %q, want %q", c.src, got, c.want)
		}
	}
}

func TestDefineArityMismatchRaisesArgumentError(t *testing.T) {
	vm := newVM(t)
	mustDefine(t, vm, "go_two", func(a, b int) int { return a + b })
	mustDefine(t, vm, "go_rest", func(a int, rest ...int) int { return a + len(rest) })

	for _, c := range []struct{ src, want string }{
		{"go_two(1)", "wrong number of arguments (given 1, expected 2)"},
		{"go_two(1, 2, 3)", "wrong number of arguments (given 3, expected 2)"},
		{"go_rest()", "wrong number of arguments (given 0, expected 1+)"},
	} {
		_, err := vm.Eval(c.src)
		var re *RubyError
		if !errors.As(err, &re) {
			t.Fatalf("%s = %v, want an ArgumentError", c.src, err)
		}
		if re.Class != "ArgumentError" || re.Message != c.want {
			t.Errorf("%s raised %s: %s, want ArgumentError: %s", c.src, re.Class, re.Message, c.want)
		}
	}

	// The Ruby side sees an ordinary exception it can rescue.
	if got := mustEval(t, vm, "begin; go_two(1); rescue ArgumentError => e; e.message; end").String(); got == "" {
		t.Error("the ArgumentError was not rescuable in Ruby")
	}
}

func TestDefineParameterKinds(t *testing.T) {
	vm := newVM(t)

	var seen []string
	note := func(format string, a ...any) { seen = append(seen, fmt.Sprintf(format, a...)) }
	mustDefine(t, vm, "go_kinds", func(
		b bool, i int, i8 int8, i16 int16, i32 int32, i64 int64,
		u uint, u8 uint8, u16 uint16, u32 uint32, u64 uint64,
		f32 float32, f64 float64, s string, sym Symbol, raw []byte,
		v *Value, list []any, hash map[any]any, named map[string]any, x any,
	) {
		note("%v %d %d %d %d %d", b, i, i8, i16, i32, i64)
		note("%d %d %d %d %d", u, u8, u16, u32, u64)
		note("%v %v %q %q %q", f32, f64, s, sym, raw)
		note("%v %v %v", v.Type(), list, hash)
		note("%v %v", named, x)
	})

	src := `go_kinds(true, 1, 2, 3, 4, 5,
		6, 7, 8, 9, 10,
		1.5, 2.5, 'text', :sym, 'bytes',
		Object.new, [1, 'two'], {1 => 2}, {'k' => 'v'}, {a: [1]})`
	if _, err := vm.Eval(src); err != nil {
		t.Fatalf("go_kinds: %v", err)
	}
	want := []string{
		"true 1 2 3 4 5",
		"6 7 8 9 10",
		"1.5 2.5 \"text\" \"sym\" \"bytes\"",
		"object [1 two] map[1:2]",
		"map[k:v] map[a:[1]]",
	}
	for i := range want {
		if i >= len(seen) || seen[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, seen[min(i, len(seen)-1)], want[i])
		}
	}

	// A Symbol reaches a string parameter as its name; a parameter typed Symbol is what tells the two apart.
	mustDefine(t, vm, "go_string", func(s string) string { return s })
	if got := mustEval(t, vm, "go_string(:from_a_symbol)").String(); got != "from_a_symbol" {
		t.Errorf("go_string(:from_a_symbol) = %q", got)
	}
}

func TestDefineParameterMismatchRaisesTypeError(t *testing.T) {
	vm := newVM(t)
	mustDefine(t, vm, "go_int", func(a int) int { return a })
	mustDefine(t, vm, "go_byte", func(a uint8) int { return int(a) })
	mustDefine(t, vm, "go_sym", func(s Symbol) Symbol { return s })
	mustDefine(t, vm, "go_list", func(xs []any) int { return len(xs) })

	for _, c := range []struct{ src, want string }{
		{"go_int('7')", "argument 1: the value is String, not an Integer"},
		{"go_int(1.5)", "argument 1: the value is Float, not an Integer"},
		{"go_byte(300)", "argument 1: 300 does not fit in a Go uint8"},
		{"go_byte(-1)", "argument 1: -1 does not fit in a Go uint8"},
		{"go_sym('text')", "argument 1: the value is String, not a Symbol"},
		{"go_list({})", "argument 1: the value is Hash, not an Array"},
	} {
		_, err := vm.Eval(c.src)
		var re *RubyError
		if !errors.As(err, &re) {
			t.Fatalf("%s = %v, want a TypeError", c.src, err)
		}
		if re.Class != "TypeError" || re.Message != c.want {
			t.Errorf("%s raised %s: %s, want TypeError: %s", c.src, re.Class, re.Message, c.want)
		}
	}
}

func TestDefineResultShapes(t *testing.T) {
	vm := newVM(t)
	var ran bool
	mustDefine(t, vm, "go_nothing", func() { ran = true })
	mustDefine(t, vm, "go_value", func() []any { return []any{1, "two"} })
	mustDefine(t, vm, "go_error", func(fail bool) error {
		if fail {
			return errors.New("it went wrong")
		}
		return nil
	})
	mustDefine(t, vm, "go_both", func(fail bool) (any, error) {
		if fail {
			return nil, Raise("KeyError", "no key %q", "k")
		}
		return map[string]any{"ok": true}, nil
	})

	if got := mustEval(t, vm, "go_nothing").Inspect(); got != "nil" {
		t.Errorf("a Go function with no result answered %s", got)
	}
	if !ran {
		t.Error("the Go function did not run")
	}
	if got := mustEval(t, vm, "go_value.inspect").String(); got != `[1, "two"]` {
		t.Errorf("go_value = %s", got)
	}
	if got := mustEval(t, vm, "go_error(false).inspect").String(); got != "nil" {
		t.Errorf("a Go function returning a nil error answered %s", got)
	}
	if got := mustEval(t, vm, "go_both(false).inspect").String(); got != `{"ok" => true}` {
		t.Errorf("go_both(false) = %s", got)
	}

	_, err := vm.Eval("go_error(true)")
	var re *RubyError
	if !errors.As(err, &re) {
		t.Fatalf("go_error(true) = %v", err)
	}
	if re.Class != "RuntimeError" || re.Message != "it went wrong" {
		t.Errorf("a plain Go error raised %s: %s", re.Class, re.Message)
	}
	_, err = vm.Eval("go_both(true)")
	if !errors.As(err, &re) {
		t.Fatalf("go_both(true) = %v", err)
	}
	if re.Class != "KeyError" || re.Message != `no key "k"` {
		t.Errorf("Raise from a Go function raised %s: %s", re.Class, re.Message)
	}
	// Ruby rescues it like any other exception, and the class is the one that was asked for.
	if got := mustEval(t, vm, "begin; go_both(true); rescue KeyError => e; e.message; end").String(); got != `no key "k"` {
		t.Errorf("rescued message = %q", got)
	}
}

func TestDefineResultOverflow(t *testing.T) {
	vm := newVM(t)
	mustDefine(t, vm, "go_big", func() uint64 { return math.MaxUint64 })
	mustDefine(t, vm, "go_fits", func() uint64 { return math.MaxInt64 })

	_, err := vm.Eval("go_big")
	var re *RubyError
	if !errors.As(err, &re) {
		t.Fatalf("go_big = %v", err)
	}
	if re.Class != "RuntimeError" || !strings.Contains(re.Message, "18446744073709551615") {
		t.Errorf("go_big raised %s: %s", re.Class, re.Message)
	}
	if got := mustEval(t, vm, "go_fits").String(); got != "9223372036854775807" {
		t.Errorf("go_fits = %s", got)
	}
}

func TestDefineRejectsWhatItCannotCall(t *testing.T) {
	vm := newVM(t)
	for _, c := range []struct {
		fn   any
		want string
	}{
		{42, "a host function is a Go func, not int"},
		{(func())(nil), "a host function is not a nil func"},
		{func(chan int) {}, "parameter 1 is a chan int, which does not cross from Ruby"},
		{func() chan int { return nil }, "the result is a chan int, which does not cross into Ruby"},
		{func() (int, int) { return 0, 0 }, "the second result is a int, which is not error"},
		{func() (int, int, int) { return 0, 0, 0 }, "a host function returns nothing, a value, an error, or a value and an error, not 3 results"},
	} {
		err := vm.Define("go_bad", c.fn)
		if err == nil {
			t.Errorf("Define(%T) succeeded", c.fn)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("Define(%T) = %v, want it to say %q", c.fn, err, c.want)
		}
	}
}

func TestDefineExplicitForm(t *testing.T) {
	vm := newVM(t)
	mustDefine(t, vm, "go_call", func(c *Call) (any, error) {
		if c.VM() != vm {
			return nil, errors.New("the call carries another VM")
		}
		parts := []any{fmt.Sprint(c.Len()), c.Self().Inspect()}
		for i := 0; i < c.Len(); i++ {
			parts = append(parts, c.Arg(i).Inspect())
		}
		if c.Arg(c.Len()) != nil {
			return nil, errors.New("an argument past the last one is there")
		}
		if b := c.Block(); b != nil {
			answer, err := b.Call("call", c.Len())
			if err != nil {
				return nil, err
			}
			parts = append(parts, "block="+answer.Inspect())
		}
		return parts, nil
	})

	got := mustEval(t, vm, "go_call(1, 'two', :three).join(' ')").String()
	if got != `3 main 1 "two" :three` {
		t.Errorf("go_call = %q", got)
	}
	got = mustEval(t, vm, "go_call(1) { |n| n * 10 }.join(' ')").String()
	if got != "1 main 1 block=10" {
		t.Errorf("go_call with a block = %q", got)
	}
	// self is the receiver, whatever it is.
	got = mustEval(t, vm, "7.instance_eval { go_call }.join(' ')").String()
	if got != "0 7" {
		t.Errorf("go_call with an Integer receiver = %q", got)
	}
}

func TestHostFunctionPanicBecomesAnException(t *testing.T) {
	vm := newVM(t)
	mustDefine(t, vm, "go_panic", func() { panic("something gave way") })

	_, err := vm.Eval("go_panic")
	var re *RubyError
	if !errors.As(err, &re) {
		t.Fatalf("go_panic = %v", err)
	}
	if re.Class != "RuntimeError" {
		t.Errorf("a panic raised %s", re.Class)
	}
	if !strings.Contains(re.Message, "something gave way") {
		t.Errorf("the message does not carry the panic: %q", re.Message)
	}
	if !strings.Contains(re.Message, "host_test.go") {
		t.Errorf("the message carries no Go stack: %q", re.Message)
	}
	if v := mustEval(t, vm, "1 + 1"); v.String() != "2" {
		t.Error("the VM did not survive a panic in a host function")
	}
}

func TestHostFunctionCallsBackIntoRuby(t *testing.T) {
	vm := newVM(t)
	mustDefine(t, vm, "go_twice", func(c *Call) (any, error) {
		doubler, err := c.VM().Eval("->(x) { x * 2 }")
		if err != nil {
			return nil, err
		}
		return doubler.Call("call", c.Arg(0))
	})
	if got := mustEval(t, vm, "go_twice(21)").String(); got != "42" {
		t.Errorf("go_twice(21) = %s", got)
	}

	// A Ruby exception raised by the nested call crosses back out as the same exception.
	mustDefine(t, vm, "go_relay", func(c *Call) (any, error) {
		return c.VM().Eval("raise KeyError, 'from inside'")
	})
	_, err := vm.Eval("go_relay")
	var re *RubyError
	if !errors.As(err, &re) {
		t.Fatalf("go_relay = %v", err)
	}
	if re.Class != "KeyError" || re.Message != "from inside" {
		t.Errorf("go_relay raised %s: %s", re.Class, re.Message)
	}
}

// Ruby calls a Go method, which calls the block it was given, and that block calls a second Go method.
func TestBlocksThreeLevels(t *testing.T) {
	vm := newVM(t)
	var innerCalls int

	mustDefine(t, vm, "go_outer", func(c *Call) (any, error) {
		block := c.Block()
		if block == nil {
			return nil, Raise("LocalJumpError", "no block given")
		}
		seed, err := c.Arg(0).AsInt()
		if err != nil {
			return nil, err
		}
		answer, err := block.Call("call", seed+1)
		if err != nil {
			return nil, err
		}
		n, err := answer.AsInt()
		if err != nil {
			return nil, err
		}
		return n * 10, nil
	})
	mustDefine(t, vm, "go_inner", func(c *Call) (any, error) {
		innerCalls++
		doubler, err := c.VM().Eval("->(x) { x * 2 }")
		if err != nil {
			return nil, err
		}
		doubled, err := doubler.Call("call", c.Arg(0))
		if err != nil {
			return nil, err
		}
		n, err := doubled.AsInt()
		if err != nil {
			return nil, err
		}
		return n + 3, nil
	})

	if got := mustEval(t, vm, "go_outer(4) { |x| go_inner(x) }").String(); got != "130" {
		t.Errorf("go_outer(4) { go_inner } = %s, want 130", got)
	}
	if innerCalls != 1 {
		t.Errorf("go_inner ran %d times, want 1", innerCalls)
	}
	if got := mustEval(t, vm, "go_outer(0) { |x| go_inner(x) }").String(); got != "50" {
		t.Errorf("the second run = %s, want 50", got)
	}
	_, err := vm.Eval("go_outer(1)")
	var re *RubyError
	if !errors.As(err, &re) || re.Class != "LocalJumpError" {
		t.Errorf("go_outer without a block = %v", err)
	}
}

func TestProcFromGo(t *testing.T) {
	vm := newVM(t)

	// A Proc is called the way any other method is called, through the name it answers to.
	proc := mustEval(t, vm, "->(a, b) { a * b }")
	answer, err := proc.Call("call", 6, 7)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := answer.AsInt(); got != 42 {
		t.Errorf("the Proc answered %d", got)
	}

	// A block a Ruby method yields to reaches Go the same way.
	mustDefine(t, vm, "go_collect", func(c *Call) (any, error) {
		var out []any
		for i := 0; i < 3; i++ {
			v, err := c.Block().Call("call", i)
			if err != nil {
				return nil, err
			}
			x, err := v.GoValue()
			if err != nil {
				return nil, err
			}
			out = append(out, x)
		}
		return out, nil
	})
	if got := mustEval(t, vm, "go_collect { |i| i * i }.inspect").String(); got != "[0, 1, 4]" {
		t.Errorf("go_collect = %s", got)
	}
}

func TestClasses(t *testing.T) {
	vm := newVM(t)

	counter, err := vm.DefineClass("Counter", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := counter.DefineMethod("initialize", func(c *Call) (any, error) {
		start := int64(0)
		if c.Len() > 0 {
			if start, err = c.Arg(0).AsInt(); err != nil {
				return nil, err
			}
		}
		return c.Self().Call("instance_variable_set", Symbol("@n"), start)
	}); err != nil {
		t.Fatal(err)
	}
	if err := counter.DefineMethod("bump", func(c *Call) (any, error) {
		held, err := c.Self().Call("instance_variable_get", Symbol("@n"))
		if err != nil {
			return nil, err
		}
		n, err := held.AsInt()
		if err != nil {
			return nil, err
		}
		if _, err := c.Self().Call("instance_variable_set", Symbol("@n"), n+1); err != nil {
			return nil, err
		}
		return n + 1, nil
	}); err != nil {
		t.Fatal(err)
	}

	instance, err := counter.New(10)
	if err != nil {
		t.Fatal(err)
	}
	for want := int64(11); want <= 13; want++ {
		v, err := instance.Call("bump")
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := v.AsInt(); got != want {
			t.Errorf("bump = %d, want %d", got, want)
		}
	}
	// Ruby sees the same class.
	if got := mustEval(t, vm, "Counter.new.bump").String(); got != "1" {
		t.Errorf("Counter.new.bump = %s", got)
	}
	if got := mustEval(t, vm, "Counter.ancestors.include?(Object).to_s").String(); got != "true" {
		t.Errorf("Counter does not derive from Object: %s", got)
	}
	if got := counter.Value().String(); got != "Counter" {
		t.Errorf("the class value is %q", got)
	}

	// A class derived from a Go-defined class inherits its methods.
	derived, err := vm.DefineClass("Doubler", counter)
	if err != nil {
		t.Fatal(err)
	}
	if err := derived.DefineMethod("twice", func(c *Call) (any, error) {
		if _, err := c.Self().Call("bump"); err != nil {
			return nil, err
		}
		return c.Self().Call("bump")
	}); err != nil {
		t.Fatal(err)
	}
	v, err := derived.New(0)
	if err != nil {
		t.Fatal(err)
	}
	twice, err := v.Call("twice")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := twice.AsInt(); got != 2 {
		t.Errorf("the derived method answered %d, want 2", got)
	}

	// A Go method reached through a Ruby method that is not there is still a NoMethodError.
	if _, err := v.Call("no_such_method"); err == nil {
		t.Error("a method that is not there answered")
	}
}
