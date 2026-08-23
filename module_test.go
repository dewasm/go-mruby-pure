package mruby

import (
	"errors"
	"strings"
	"testing"
)

func mustDefineModule(t *testing.T, vm *VM, name string) *Module {
	t.Helper()
	m, err := vm.DefineModule(name)
	if err != nil {
		t.Fatalf("DefineModule(%q): %v", name, err)
	}
	return m
}

func mustDefineClass(t *testing.T, vm *VM, name string, super *Class) *Class {
	t.Helper()
	c, err := vm.DefineClass(name, super)
	if err != nil {
		t.Fatalf("DefineClass(%q): %v", name, err)
	}
	return c
}

func TestModuleFunctionAndInclude(t *testing.T) {
	vm := newVM(t)

	greetings := mustDefineModule(t, vm, "Greetings")
	if err := greetings.DefineModuleFunction("shout", func(s string) string {
		return strings.ToUpper(s) + "!"
	}); err != nil {
		t.Fatal(err)
	}
	if err := greetings.DefineMethod("greet", func(s string) string {
		return "hello, " + s
	}); err != nil {
		t.Fatal(err)
	}

	if got := greetings.Value().String(); got != "Greetings" {
		t.Errorf("the module value is %q", got)
	}
	if got := mustEval(t, vm, "Greetings.class.to_s").String(); got != "Module" {
		t.Errorf("Greetings is a %s", got)
	}
	if got := mustEval(t, vm, `Greetings.shout("go")`).String(); got != "GO!" {
		t.Errorf("Greetings.shout = %q", got)
	}

	// The instance methods reach a class that includes the module, the module function among them.
	mustEval(t, vm, "class Host; include Greetings; end")
	if got := mustEval(t, vm, `Host.new.greet("world")`).String(); got != "hello, world" {
		t.Errorf("the included method answered %q", got)
	}
	if got := mustEval(t, vm, `Host.new.shout("in")`).String(); got != "IN!" {
		t.Errorf("the included module function answered %q", got)
	}

	// A module is not a class: it has no instances of its own.
	if _, err := vm.Eval("Greetings.new"); err == nil {
		t.Error("Greetings.new answered")
	}
}

func TestNestedDefinitions(t *testing.T) {
	vm := newVM(t)

	namespace := mustDefineModule(t, vm, "Namespace")
	inner, err := namespace.DefineClass("Inner", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := inner.DefineMethod("who", func() string { return "inner" }); err != nil {
		t.Fatal(err)
	}

	if got := mustEval(t, vm, "Namespace::Inner.to_s").String(); got != "Namespace::Inner" {
		t.Errorf("the nested class is called %q", got)
	}
	if got := mustEval(t, vm, "Namespace::Inner.new.who").String(); got != "inner" {
		t.Errorf("Namespace::Inner.new.who = %q", got)
	}
	instance, err := inner.New()
	if err != nil {
		t.Fatal(err)
	}
	who, err := instance.Call("who")
	if err != nil {
		t.Fatal(err)
	}
	if got := who.String(); got != "inner" {
		t.Errorf("the instance made from Go answered %q", got)
	}

	// A module nests in a module, a class nests in a class, and a nested class derives from a nested one.
	deeper, err := namespace.DefineModule("Deeper")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deeper.DefineClass("Leaf", inner); err != nil {
		t.Fatal(err)
	}
	if _, err := inner.DefineClass("Nested", nil); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ src, want string }{
		{"Namespace::Deeper.to_s", "Namespace::Deeper"},
		{"Namespace::Deeper::Leaf.new.who", "inner"},
		{"Namespace::Deeper::Leaf.superclass.to_s", "Namespace::Inner"},
		{"Namespace::Inner::Nested.to_s", "Namespace::Inner::Nested"},
	} {
		if got := mustEval(t, vm, c.src).String(); got != c.want {
			t.Errorf("%s = %q, want %q", c.src, got, c.want)
		}
	}
}

func TestDefineClassMethod(t *testing.T) {
	vm := newVM(t)

	registry := mustDefineClass(t, vm, "Registry", nil)
	if err := registry.DefineClassMethod("build", func() string { return "built" }); err != nil {
		t.Fatal(err)
	}
	if err := registry.DefineMethod("kind", func() string { return "instance" }); err != nil {
		t.Fatal(err)
	}

	if got := mustEval(t, vm, "Registry.build").String(); got != "built" {
		t.Errorf("Registry.build = %q", got)
	}
	built, err := registry.Value().Call("build")
	if err != nil {
		t.Fatal(err)
	}
	if got := built.String(); got != "built" {
		t.Errorf("the class method called from Go answered %q", got)
	}

	// A class method is not an instance method.
	_, err = vm.Eval("Registry.new.build")
	var re *RubyError
	if !errors.As(err, &re) || re.Class != "NoMethodError" {
		t.Errorf("Registry.new.build = %v, want a NoMethodError", err)
	}
	if got := mustEval(t, vm, "Registry.new.kind").String(); got != "instance" {
		t.Errorf("the instance method answered %q", got)
	}
}

func TestDefineConst(t *testing.T) {
	vm := newVM(t)

	if err := vm.DefineConst("TOP", 7); err != nil {
		t.Fatal(err)
	}
	if err := vm.DefineConst("LIST", []any{1, "two"}); err != nil {
		t.Fatal(err)
	}
	config := mustDefineClass(t, vm, "Config", nil)
	if err := config.DefineConst("LIMIT", 5); err != nil {
		t.Fatal(err)
	}
	units := mustDefineModule(t, vm, "Units")
	if err := units.DefineConst("NAME", "metre"); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct{ src, want string }{
		{"TOP", "7"},
		{"LIST.inspect", `[1, "two"]`},
		{"Config::LIMIT", "5"},
		{"Units::NAME", "metre"},
	} {
		if got := mustEval(t, vm, c.src).String(); got != c.want {
			t.Errorf("%s = %q, want %q", c.src, got, c.want)
		}
	}

	for _, c := range []struct{ path, want string }{
		{"TOP", "7"},
		{"Config::LIMIT", "5"},
		{"Units::NAME", "metre"},
	} {
		v, err := vm.Constant(c.path)
		if err != nil {
			t.Fatalf("Constant(%q): %v", c.path, err)
		}
		if got := v.String(); got != c.want {
			t.Errorf("Constant(%q) = %q, want %q", c.path, got, c.want)
		}
	}

	// The name crosses as it is: a lowercase one names a constant Ruby syntax cannot reach, still readable through Constant.
	if err := vm.DefineConst("lower", 1); err != nil {
		t.Fatal(err)
	}
	lower, err := vm.Constant("lower")
	if err != nil {
		t.Fatal(err)
	}
	if got := lower.String(); got != "1" {
		t.Errorf("Constant(\"lower\") = %s, want 1", got)
	}
}
