package mruby

import (
	"math"
	"strings"
	"testing"
)

func TestExtractorsAreStrict(t *testing.T) {
	vm := newVM(t)

	if _, err := mustEval(t, vm, "1.5").Int(); err == nil {
		t.Error("Int of a Float succeeded")
	}
	if _, err := mustEval(t, vm, "'7'").Int(); err == nil {
		t.Error("Int of a String succeeded")
	}
	if f, err := mustEval(t, vm, "7").Float(); err != nil || f != 7 {
		t.Errorf("Float of an Integer = %v, %v", f, err)
	}
	if _, err := mustEval(t, vm, "nil").Bool(); err == nil {
		t.Error("Bool of nil succeeded; truthiness is not what Bool reports")
	}
	if _, err := mustEval(t, vm, "1").Bool(); err == nil {
		t.Error("Bool of an Integer succeeded")
	}
	if b, err := mustEval(t, vm, "false").Bool(); err != nil || b {
		t.Errorf("Bool of false = %v, %v", b, err)
	}
	if s, err := mustEval(t, vm, ":sym").Text(); err != nil || s != "sym" {
		t.Errorf("Text of a Symbol = %q, %v", s, err)
	}
	if _, err := mustEval(t, vm, "[]").Text(); err == nil {
		t.Error("Text of an Array succeeded")
	}
	if got := mustEval(t, vm, "1.5").typeError("an Integer").Error(); !strings.Contains(got, "Float") {
		t.Errorf("the type error does not name the type: %s", got)
	}

	if !mustEval(t, vm, "nil").IsNil() {
		t.Error("nil is not IsNil")
	}
	if mustEval(t, vm, "false").IsNil() {
		t.Error("false reads as nil")
	}
	var absent *Value
	if !absent.IsNil() || absent.Type() != TypeNil {
		t.Error("a value that is not there does not read as nil")
	}
	if _, err := absent.Int(); err == nil {
		t.Error("Int of a value that is not there succeeded")
	}
	if _, err := absent.Call("to_s"); err == nil {
		t.Error("Call on a value that is not there succeeded")
	}
	if x, err := absent.Export(); x != nil || err != nil {
		t.Errorf("Export of a value that is not there = %v, %v", x, err)
	}
}

// String and Inspect answer what Ruby answers, which is what makes them usable in a message.
func TestStringAndInspectMatchRuby(t *testing.T) {
	vm := newVM(t)
	for _, src := range []string{
		"nil", "true", "false",
		"0", "42", "-7", "9223372036854775807", "-9223372036854775807 - 1",
		"0.0", "-0.0", "1.0", "3.5", "0.1", "1.0 / 3", "1e14", "1e15", "1e16", "1e-5", "1e100",
		"-2.5e-7", "1234567890.5", "Float::INFINITY", "-Float::INFINITY", "Float::NAN",
		"'text'", "''", `"a\tb"`, "'日本語'", ":sym", `:"a symbol"`,
		"[1, :two, nil]", "{a: 1}", "(1..3).to_a",
	} {
		v := mustEval(t, vm, src)
		want := mustEval(t, vm, "("+src+").to_s")
		if got, _ := want.Text(); v.String() != got {
			t.Errorf("(%s).String() = %q, Ruby says %q", src, v.String(), got)
		}
		want = mustEval(t, vm, "("+src+").inspect")
		if got, _ := want.Text(); v.Inspect() != got {
			t.Errorf("(%s).Inspect() = %q, Ruby says %q", src, v.Inspect(), got)
		}
	}
}

func TestFormatFloatMatchesRuby(t *testing.T) {
	vm := newVM(t)
	for _, f := range []float64{
		0, math.Copysign(0, -1), 1, -1, 3.5, 0.1, 1.0 / 3.0,
		1e-3, 1e-4, 1e-5, 1e14, 1e15, 1e16, 1e17, 1e20, 1e100,
		-2.5e-7, 1234567890.5, math.MaxFloat64, math.SmallestNonzeroFloat64,
		math.NaN(), math.Inf(1), math.Inf(-1),
	} {
		v, err := vm.Call(nil, "identity", f)
		if err != nil {
			t.Fatal(err)
		}
		want, err := v.Call("to_s")
		if err != nil {
			t.Fatal(err)
		}
		got, _ := want.Text()
		if formatFloat(f) != got {
			t.Errorf("formatFloat(%v) = %q, Ruby says %q", f, formatFloat(f), got)
		}
	}
}

func TestStringOfARaisingToS(t *testing.T) {
	vm := newVM(t)
	v := mustEval(t, vm, "class Silent; def to_s; raise 'no to_s'; end; end; Silent.new")
	if got := v.String(); !strings.Contains(got, "no to_s") {
		t.Errorf("String of a raising to_s = %q, want the error in it", got)
	}
	if got := v.Inspect(); !strings.Contains(got, "Silent") {
		t.Errorf("Inspect of the same object = %q", got)
	}
}

func TestExportNested(t *testing.T) {
	vm := newVM(t)
	x, err := mustEval(t, vm, `[1, 2.5, 'three', :four, nil, true, [5, [6]], {'k' => [7]}]`).Export()
	if err != nil {
		t.Fatal(err)
	}
	want := []any{
		int64(1), 2.5, "three", Symbol("four"), nil, true,
		[]any{int64(5), []any{int64(6)}},
		map[any]any{"k": []any{int64(7)}},
	}
	if !equalAny(x, want) {
		t.Errorf("Export = %#v, want %#v", x, want)
	}
}

func TestExportHashKeysOfEveryKind(t *testing.T) {
	vm := newVM(t)
	x, err := mustEval(t, vm, `{'s' => 1, :y => 2, 3 => 3, 4.5 => 4, nil => 5, true => 6, [7] => 7}`).Export()
	if err != nil {
		t.Fatal(err)
	}
	m, ok := x.(map[any]any)
	if !ok {
		t.Fatalf("Export of a Hash = %T", x)
	}
	for key, want := range map[any]any{"s": int64(1), Symbol("y"): int64(2), int64(3): int64(3), 4.5: int64(4), nil: int64(5), true: int64(6)} {
		if got, ok := m[key]; !ok || !equalAny(got, want) {
			t.Errorf("key %#v = %#v, want %#v", key, got, want)
		}
	}
	// An Array key has no Go counterpart to be a map key, so the Value stands for it.
	var arrayKeys int
	for key, value := range m {
		v, ok := key.(*Value)
		if !ok {
			continue
		}
		arrayKeys++
		if v.Type() != TypeArray {
			t.Errorf("the key that stayed a Value is %v", v.Type())
		}
		if !equalAny(value, int64(7)) {
			t.Errorf("the Array key holds %#v", value)
		}
	}
	if arrayKeys != 1 {
		t.Errorf("%d keys stayed Values, want 1", arrayKeys)
	}
}

func TestExportKeepsWhatItCannotConvert(t *testing.T) {
	vm := newVM(t)
	x, err := mustEval(t, vm, "[1, Object.new, {k: ->(a) { a }}]").Export()
	if err != nil {
		t.Fatal(err)
	}
	xs, ok := x.([]any)
	if !ok || len(xs) != 3 {
		t.Fatalf("Export = %#v", x)
	}
	obj, ok := xs[1].(*Value)
	if !ok || obj.Type() != TypeObject {
		t.Fatalf("element 1 = %#v, want a *Value holding an object", xs[1])
	}
	if !strings.Contains(obj.Inspect(), "Object") {
		t.Errorf("the kept object inspects as %q", obj.Inspect())
	}
	// The Value inside the tree is still live: nothing released it on the way out.
	if _, err := obj.Call("class"); err != nil {
		t.Errorf("calling the kept object: %v", err)
	}
	proc, ok := xs[2].(map[any]any)[Symbol("k")].(*Value)
	if !ok {
		t.Fatalf("the Proc in the Hash = %#v", xs[2])
	}
	answer, err := proc.Call("call", 9)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := answer.Int(); n != 9 {
		t.Errorf("the kept Proc answered %d", n)
	}
}

func TestExportRejectsCycles(t *testing.T) {
	vm := newVM(t)
	for _, src := range []string{
		"a = []; a << a; a",
		"h = {}; h[:self] = h; h",
		"a = []; b = [a]; a << b; a",
	} {
		_, err := mustEval(t, vm, src).Export()
		if err == nil {
			t.Errorf("Export(%s) succeeded on a structure that contains itself", src)
			continue
		}
		if !strings.Contains(err.Error(), "contains itself") {
			t.Errorf("Export(%s) failed with %v", src, err)
		}
	}

	// The same object twice side by side is not a cycle.
	x, err := mustEval(t, vm, "x = [1]; [x, x]").Export()
	if err != nil {
		t.Fatalf("Export of a shared element: %v", err)
	}
	if !equalAny(x, []any{[]any{int64(1)}, []any{int64(1)}}) {
		t.Errorf("Export of a shared element = %#v", x)
	}
}

func TestReleaseEndsTheValue(t *testing.T) {
	vm := newVM(t)

	v := mustEval(t, vm, "[1, 2]")
	v.Release()
	v.Release()
	if _, err := v.Call("size"); err == nil {
		t.Error("a call on a released value succeeded")
	}
	if _, err := v.Export(); err == nil {
		t.Error("Export of a released value succeeded")
	}
	if got := v.String(); !strings.Contains(got, "released") {
		t.Errorf("String of a released value = %q", got)
	}
	if _, err := vm.Call(nil, "identity", v); err == nil {
		t.Error("a released value crossed into Ruby")
	}

	// An immediate holds no reference, so releasing it is nothing.
	imm := mustEval(t, vm, "42")
	imm.Release()
	if n, err := imm.Int(); err != nil || n != 42 {
		t.Errorf("an immediate did not survive Release: %d, %v", n, err)
	}
	if v := mustEval(t, vm, "'still here'"); v.String() != "still here" {
		t.Error("the VM did not survive a released value")
	}
}
