package mruby

import (
	"fmt"
	"log"
	"strings"
	"testing"
)

func TestDecodeStructFromHashStringKeys(t *testing.T) {
	vm := newVM(t)
	v := mustEval(t, vm, `{"name" => "mruby", "age" => 4}`)

	type Person struct {
		Name string
		Age  int
	}
	var p Person
	if err := Decode(&p, v); err != nil {
		t.Fatal(err)
	}
	if p.Name != "mruby" || p.Age != 4 {
		t.Errorf("Decode = %+v", p)
	}
}

func TestDecodeStructFromHashSymbolKeys(t *testing.T) {
	vm := newVM(t)
	v := mustEval(t, vm, `{name: "mruby", age: 4}`)

	type Person struct {
		Name string
		Age  int
	}
	var p Person
	if err := Decode(&p, v); err != nil {
		t.Fatal(err)
	}
	if p.Name != "mruby" || p.Age != 4 {
		t.Errorf("Decode = %+v", p)
	}
}

func TestDecodeIntOverflow(t *testing.T) {
	vm := newVM(t)
	v := mustEval(t, vm, "300")

	var n8 int8
	if err := Decode(&n8, v); err == nil {
		t.Error("300 fit in an int8")
	}
	var n64 int64
	if err := Decode(&n64, v); err != nil {
		t.Fatal(err)
	}
	if n64 != 300 {
		t.Errorf("n64 = %d", n64)
	}
}

func TestDecodeUintNegative(t *testing.T) {
	vm := newVM(t)
	v := mustEval(t, vm, "-1")

	var n uint
	if err := Decode(&n, v); err == nil {
		t.Error("-1 decoded into a uint")
	}
}

func TestDecodeFloatFromInteger(t *testing.T) {
	vm := newVM(t)
	v := mustEval(t, vm, "7")

	var f float64
	if err := Decode(&f, v); err != nil {
		t.Fatal(err)
	}
	if f != 7 {
		t.Errorf("f = %v", f)
	}
}

func TestDecodeStringFromSymbol(t *testing.T) {
	vm := newVM(t)
	v := mustEval(t, vm, ":ok")

	var s string
	if err := Decode(&s, v); err != nil {
		t.Fatal(err)
	}
	if s != "ok" {
		t.Errorf("s = %q", s)
	}
}

func TestDecodeSymbolField(t *testing.T) {
	vm := newVM(t)
	v := mustEval(t, vm, `{status: :ok}`)

	type Result struct {
		Status Symbol
	}
	var r Result
	if err := Decode(&r, v); err != nil {
		t.Fatal(err)
	}
	if r.Status != Symbol("ok") {
		t.Errorf("Status = %q", r.Status)
	}

	// A Symbol field rejects a String.
	v2 := mustEval(t, vm, `{status: "ok"}`)
	var r2 Result
	if err := Decode(&r2, v2); err == nil {
		t.Error("a String decoded into a Symbol field")
	}
}

func TestDecodeNestedStruct(t *testing.T) {
	vm := newVM(t)
	v := mustEval(t, vm, `{name: "mruby", release: {major: 4, minor: 0}}`)

	type Release struct {
		Major, Minor int
	}
	type Gem struct {
		Name    string
		Release Release
	}
	var g Gem
	if err := Decode(&g, v); err != nil {
		t.Fatal(err)
	}
	if g.Name != "mruby" || g.Release.Major != 4 || g.Release.Minor != 0 {
		t.Errorf("Decode = %+v", g)
	}
}

func TestDecodeSliceOfStructs(t *testing.T) {
	vm := newVM(t)
	v := mustEval(t, vm, `[{name: "a"}, {name: "b"}]`)

	type Item struct {
		Name string
	}
	var items []Item
	if err := Decode(&items, v); err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].Name != "a" || items[1].Name != "b" {
		t.Errorf("items = %+v", items)
	}
}

func TestDecodeMapStringAny(t *testing.T) {
	vm := newVM(t)
	v := mustEval(t, vm, `{"a" => 1, "b" => "two"}`)

	var m map[string]any
	if err := Decode(&m, v); err != nil {
		t.Fatal(err)
	}
	if m["a"] != int64(1) || m["b"] != "two" {
		t.Errorf("m = %#v", m)
	}
}

// TestDecodeMapSymbolInt covers map[Symbol]int, which falls out of the same
// machinery as string keys: a Symbol target key only accepts a Ruby Symbol,
// and {a: 1, b: 2} supplies exactly that.
func TestDecodeMapSymbolInt(t *testing.T) {
	vm := newVM(t)
	v := mustEval(t, vm, `{a: 1, b: 2}`)

	var m map[Symbol]int
	if err := Decode(&m, v); err != nil {
		t.Fatal(err)
	}
	if m[Symbol("a")] != 1 || m[Symbol("b")] != 2 {
		t.Errorf("m = %#v", m)
	}
}

func TestDecodeMapBadKeyType(t *testing.T) {
	vm := newVM(t)
	v := mustEval(t, vm, `{1 => "one"}`)

	var m map[int]string
	if err := Decode(&m, v); err == nil {
		t.Error("a map[int]string decoded from a Hash")
	}
}

func TestDecodePointerField(t *testing.T) {
	vm := newVM(t)

	type Box struct {
		Value *int
	}
	var withValue Box
	if err := Decode(&withValue, mustEval(t, vm, "{value: 42}")); err != nil {
		t.Fatal(err)
	}
	if withValue.Value == nil || *withValue.Value != 42 {
		t.Errorf("Value = %v", withValue.Value)
	}

	var withNil Box
	if err := Decode(&withNil, mustEval(t, vm, "{value: nil}")); err != nil {
		t.Fatal(err)
	}
	if withNil.Value != nil {
		t.Errorf("Value = %v, want nil", withNil.Value)
	}
}

func TestDecodeAnyField(t *testing.T) {
	vm := newVM(t)
	for _, c := range []struct {
		src  string
		want any
	}{
		{"nil", nil},
		{"true", true},
		{"42", int64(42)},
		{"1.5", 1.5},
		{`"text"`, "text"},
		{":sym", Symbol("sym")},
	} {
		var x any
		if err := Decode(&x, mustEval(t, vm, c.src)); err != nil {
			t.Fatalf("Decode(%q): %v", c.src, err)
		}
		if x != c.want {
			t.Errorf("Decode(%q) = %#v, want %#v", c.src, x, c.want)
		}
	}

	var arr any
	if err := Decode(&arr, mustEval(t, vm, "[1, 2]")); err != nil {
		t.Fatal(err)
	}
	if !equalAny(arr, []any{int64(1), int64(2)}) {
		t.Errorf("arr = %#v", arr)
	}

	var obj any
	if err := Decode(&obj, mustEval(t, vm, "Object.new")); err != nil {
		t.Fatal(err)
	}
	if _, ok := obj.(*Value); !ok {
		t.Errorf("obj = %#v, want a *Value", obj)
	}
}

func TestDecodeValueField(t *testing.T) {
	vm := newVM(t)

	type Holder struct {
		Object *Value
	}
	var withObject Holder
	if err := Decode(&withObject, mustEval(t, vm, `{object: Object.new}`)); err != nil {
		t.Fatal(err)
	}
	if withObject.Object == nil || withObject.Object.Type() != TypeObject {
		t.Errorf("Object = %v, want an object", withObject.Object)
	}

	var withNil Holder
	if err := Decode(&withNil, mustEval(t, vm, `{object: nil}`)); err != nil {
		t.Fatal(err)
	}
	if withNil.Object != nil {
		t.Errorf("Object = %v, want nil", withNil.Object)
	}

	// Decode a *Value field directly, outside a struct, to receive the value itself.
	v := mustEval(t, vm, "Object.new")
	var direct *Value
	if err := Decode(&direct, v); err != nil {
		t.Fatal(err)
	}
	if direct != v {
		t.Errorf("direct = %v, want the same *Value", direct)
	}
}

func TestDecodeSquash(t *testing.T) {
	vm := newVM(t)
	v := mustEval(t, vm, `{name: "mruby", age: 4}`)

	type Base struct {
		Name string
	}
	type Extended struct {
		Base `mruby:",squash"`
		Age  int
	}
	var e Extended
	if err := Decode(&e, v); err != nil {
		t.Fatal(err)
	}
	if e.Name != "mruby" || e.Age != 4 {
		t.Errorf("Decode = %+v", e)
	}
}

func TestDecodeTagRenameAndSkip(t *testing.T) {
	vm := newVM(t)
	v := mustEval(t, vm, `{full_name: "mruby", secret: "nope"}`)

	type Named struct {
		Name   string `mruby:"full_name"`
		Secret string `mruby:"-"`
	}
	var n Named
	if err := Decode(&n, v); err != nil {
		t.Fatal(err)
	}
	if n.Name != "mruby" {
		t.Errorf("Name = %q", n.Name)
	}
	if n.Secret != "" {
		t.Errorf("Secret = %q, want it skipped", n.Secret)
	}
}

func TestDecodeFromObjectMethods(t *testing.T) {
	vm := newVM(t)
	mustEval(t, vm, `
		class Person
			def initialize(name, age)
				@name = name
				@age = age
			end
			def name; @name; end
			def age; @age; end
		end
	`)
	v := mustEval(t, vm, `Person.new("Ada", 32)`)

	type Person struct {
		Name    string
		Age     int
		Missing string
	}
	var p Person
	if err := Decode(&p, v); err != nil {
		t.Fatal(err)
	}
	if p.Name != "Ada" || p.Age != 32 {
		t.Errorf("Decode = %+v", p)
	}
	if p.Missing != "" {
		t.Errorf("Missing = %q, want it untouched by a NoMethodError", p.Missing)
	}
}

func TestDecodeFromObjectMethodRaises(t *testing.T) {
	vm := newVM(t)
	mustEval(t, vm, `
		class Bomb
			def name
				raise ArgumentError, "boom"
			end
		end
	`)
	v := mustEval(t, vm, `Bomb.new`)

	type Named struct {
		Name string
	}
	var n Named
	err := Decode(&n, v)
	if err == nil {
		t.Fatal("a raising method did not surface an error")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error = %v, want it to carry the Ruby message", err)
	}
}

func TestDecodeErrors(t *testing.T) {
	vm := newVM(t)

	if err := Decode(nil, mustEval(t, vm, "1")); err == nil {
		t.Error("Decode(nil, ...) succeeded")
	}

	var notPointer int
	if err := Decode(notPointer, mustEval(t, vm, "1")); err == nil {
		t.Error("Decode of a non-pointer succeeded")
	}

	var n int
	if err := Decode(&n, mustEval(t, vm, `"text"`)); err == nil {
		t.Error("a String decoded into an int")
	}

	if err := Decode(&n, mustEval(t, vm, "nil")); err == nil {
		t.Error("Ruby nil decoded into an int")
	}
}

func TestDecodeFieldPathNamesTheFailure(t *testing.T) {
	vm := newVM(t)
	v := mustEval(t, vm, `{versions: [4, "oops"]}`)

	type Gem struct {
		Versions []int
	}
	var g Gem
	err := Decode(&g, v)
	if err == nil {
		t.Fatal("a String in an []int slice decoded")
	}
	if !strings.Contains(err.Error(), "field versions[1]:") {
		t.Errorf("error = %v, want it to name field versions[1]", err)
	}
}

func ExampleDecode() {
	vm, err := New()
	if err != nil {
		log.Fatal(err)
	}
	v, err := vm.Eval(`{name: "mruby", versions: [4, 0]}`)
	if err != nil {
		log.Fatal(err)
	}

	var gem struct {
		Name     string
		Versions []int
	}
	if err := Decode(&gem, v); err != nil {
		log.Fatal(err)
	}
	fmt.Println(gem.Name, gem.Versions)
	// Output: mruby [4 0]
}
