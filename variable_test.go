package mruby

import (
	"errors"
	"strings"
	"testing"
)

func constantTree(t *testing.T, vm *VM) {
	t.Helper()
	outer := mustDefineModule(t, vm, "Outer")
	inner, err := outer.DefineModule("Inner")
	if err != nil {
		t.Fatal(err)
	}
	if err := inner.DefineConst("DEEP", 3); err != nil {
		t.Fatal(err)
	}
	if err := vm.DefineConst("PLAIN", 1); err != nil {
		t.Fatal(err)
	}
}

func TestConstantPath(t *testing.T) {
	vm := newVM(t)
	constantTree(t, vm)

	for _, c := range []struct{ path, want string }{
		{"PLAIN", "1"},
		{"Outer::Inner::DEEP", "3"},
		{"Outer::Inner", "Outer::Inner"},
	} {
		v, err := vm.Constant(c.path)
		if err != nil {
			t.Fatalf("Constant(%q): %v", c.path, err)
		}
		if got := v.String(); got != c.want {
			t.Errorf("Constant(%q) = %q, want %q", c.path, got, c.want)
		}
	}

	// A constant that is not there is the interpreter's NameError, wherever along the path it is missing.
	for _, path := range []string{"Nope", "Outer::Nope", "Outer::Inner::Nope"} {
		_, err := vm.Constant(path)
		var re *RubyError
		if !errors.As(err, &re) || re.Class != "NameError" {
			t.Errorf("Constant(%q) = %v, want a NameError", path, err)
		}
	}

	// A path this package cannot make sense of never reaches the interpreter.
	for _, path := range []string{"", "Outer::", "::Outer", "Outer::::Inner"} {
		_, err := vm.Constant(path)
		var re *RubyError
		if err == nil || errors.As(err, &re) {
			t.Errorf("Constant(%q) = %v, want a plain error", path, err)
		}
	}
}

func TestConstDefined(t *testing.T) {
	vm := newVM(t)
	constantTree(t, vm)

	for _, c := range []struct {
		path string
		want bool
	}{
		{"PLAIN", true},
		{"Outer", true},
		{"Outer::Inner", true},
		{"Outer::Inner::DEEP", true},
		{"Nope", false},
		{"Outer::Nope", false},
		{"Nope::Inner", false},
		{"Outer::Inner::Nope", false},
	} {
		got, err := vm.ConstDefined(c.path)
		if err != nil {
			t.Fatalf("ConstDefined(%q): %v", c.path, err)
		}
		if got != c.want {
			t.Errorf("ConstDefined(%q) = %v, want %v", c.path, got, c.want)
		}
	}

	// A segment that is there but is no namespace is the interpreter's TypeError, from looking a constant up in it.
	_, err := vm.ConstDefined("PLAIN::X")
	var re *RubyError
	if !errors.As(err, &re) || re.Class != "TypeError" {
		t.Errorf("ConstDefined(\"PLAIN::X\") = %v, want a TypeError", err)
	}
	if _, err := vm.ConstDefined(""); err == nil {
		t.Error("ConstDefined of an empty path answered")
	}
}

func TestGlobalVarRoundTrip(t *testing.T) {
	vm := newVM(t)

	if err := vm.SetGlobalVar("$from_go", []any{1, 2}); err != nil {
		t.Fatal(err)
	}
	if got := mustEval(t, vm, "$from_go.inspect").String(); got != "[1, 2]" {
		t.Errorf("Ruby sees $from_go as %s", got)
	}
	mustEval(t, vm, "$from_ruby = 'text'")
	v, err := vm.GlobalVar("$from_ruby")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := v.AsString(); got != "text" {
		t.Errorf("$from_ruby = %q", got)
	}

	// A global variable that was never set is nil, as in Ruby.
	unset, err := vm.GlobalVar("$never_set")
	if err != nil {
		t.Fatal(err)
	}
	if !unset.IsNil() {
		t.Errorf("$never_set = %s, want nil", unset.Inspect())
	}

	// The `$` is checked here rather than in the interpreter.
	if _, err := vm.GlobalVar("from_go"); err == nil || !strings.Contains(err.Error(), "$") {
		t.Errorf("GlobalVar(\"from_go\") = %v", err)
	}
	if err := vm.SetGlobalVar("from_go", 1); err == nil || !strings.Contains(err.Error(), "$") {
		t.Errorf("SetGlobalVar(\"from_go\") = %v", err)
	}
}

func TestInstanceVarRoundTrip(t *testing.T) {
	vm := newVM(t)

	box := mustDefineClass(t, vm, "Box", nil)
	if err := box.DefineMethod("held", func(c *Call) (any, error) {
		return c.Self().InstanceVar("@held")
	}); err != nil {
		t.Fatal(err)
	}
	instance, err := box.New()
	if err != nil {
		t.Fatal(err)
	}

	if err := instance.SetInstanceVar("@held", map[string]any{"k": 1}); err != nil {
		t.Fatal(err)
	}
	held, err := instance.InstanceVar("@held")
	if err != nil {
		t.Fatal(err)
	}
	if got := held.Inspect(); got != `{"k" => 1}` {
		t.Errorf("@held = %s", got)
	}
	// Ruby and the Go method that reads it see the same variable.
	if got := mustEval(t, vm, "Box.new.instance_variable_set(:@held, 7).to_s").String(); got != "7" {
		t.Errorf("Ruby set @held to %s", got)
	}
	answer, err := instance.Call("held")
	if err != nil {
		t.Fatal(err)
	}
	if got := answer.Inspect(); got != `{"k" => 1}` {
		t.Errorf("the Go method read @held as %s", got)
	}

	unset, err := instance.InstanceVar("@never_set")
	if err != nil {
		t.Fatal(err)
	}
	if !unset.IsNil() {
		t.Errorf("@never_set = %s, want nil", unset.Inspect())
	}

	// The name crosses as it is, so the interpreter is what refuses one without a `@`.
	var re *RubyError
	if _, err := instance.InstanceVar("held"); !errors.As(err, &re) || re.Class != "NameError" {
		t.Errorf("InstanceVar(\"held\") = %v, want a NameError", err)
	}
	if err := instance.SetInstanceVar("held", 1); !errors.As(err, &re) || re.Class != "NameError" {
		t.Errorf("SetInstanceVar(\"held\") = %v, want a NameError", err)
	}
}

func TestInstanceVarOnImmediatesAndFrozenValues(t *testing.T) {
	vm := newVM(t)

	// An immediate is a receiver like any other: it takes a reference of its own for the crossing.
	number := mustEval(t, vm, "42")
	unset, err := number.InstanceVar("@x")
	if err != nil {
		t.Fatal(err)
	}
	if !unset.IsNil() {
		t.Errorf("an Integer answers @x as %s, want nil", unset.Inspect())
	}

	// Setting one is what the interpreter refuses, and the refusal crosses as the exception it is.
	var re *RubyError
	if err := number.SetInstanceVar("@x", 1); !errors.As(err, &re) || re.Class != "ArgumentError" {
		t.Errorf("SetInstanceVar on an Integer = %v, want an ArgumentError", err)
	}
	frozen := mustEval(t, vm, "Object.new.freeze")
	if err := frozen.SetInstanceVar("@x", 1); !errors.As(err, &re) || re.Class != "FrozenError" {
		t.Errorf("SetInstanceVar on a frozen object = %v, want a FrozenError", err)
	}
	if got := mustEval(t, vm, "1 + 1").String(); got != "2" {
		t.Error("the VM did not survive a refused instance variable")
	}
}
