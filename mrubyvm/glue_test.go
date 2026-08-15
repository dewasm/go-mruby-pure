package mrubyvm

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"testing"
)

// hostTable dispatches host callbacks by the id DefineMethod registered.
type hostTable map[int32]func(*Instance) int32

func newVM(t *testing.T, stdout, stderr io.Writer, table hostTable) *Instance {
	t.Helper()
	in, err := NewInstance(stdout, stderr, func(vm *Instance) int32 {
		fn, ok := table[vm.HostFnID()]
		if !ok {
			vm.HostRaise("RuntimeError", fmt.Sprintf("no handler for fn %d", vm.HostFnID()))
			return 1
		}
		return fn(vm)
	})
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	return in
}

func mustEval(t *testing.T, in *Instance, src string, wantRef bool) Value {
	t.Helper()
	v, err := in.Eval(src, wantRef)
	if err != nil {
		t.Fatalf("Eval(%q): %v", src, err)
	}
	return v
}

func TestInitIsIdempotent(t *testing.T) {
	in := newVM(t, nil, nil, nil)
	if rc := in.fn.vmInit(); rc != 0 {
		t.Fatalf("second dm_init returned %d, want 0", rc)
	}
	if v := mustEval(t, in, "1 + 1", false); v.Int != 2 {
		t.Fatalf("after a second dm_init: 1 + 1 = %d", v.Int)
	}
}

func TestEvalScalars(t *testing.T) {
	in := newVM(t, nil, nil, nil)
	for _, c := range []struct {
		src  string
		want Value
	}{
		{"nil", Value{Kind: KindNil}},
		{"true", Value{Kind: KindTrue}},
		{"false", Value{Kind: KindFalse}},
		{"42", Value{Kind: KindInt, Int: 42}},
		{"-7", Value{Kind: KindInt, Int: -7}},
		{"9223372036854775807", Value{Kind: KindInt, Int: math.MaxInt64}},
		{"-9223372036854775807 - 1", Value{Kind: KindInt, Int: math.MinInt64}},
		{"3.5", Value{Kind: KindFloat, Float: 3.5}},
		{"'hello'", Value{Kind: KindString, Str: "hello"}},
		{":sym", Value{Kind: KindSymbol, Str: "sym"}},
		{"[1, 2]", Value{Kind: KindArray}},
		{"{a: 1}", Value{Kind: KindHash}},
		{"Object.new", Value{Kind: KindOther}},
	} {
		got := mustEval(t, in, c.src, false)
		if got != c.want {
			t.Errorf("Eval(%q) = %+v, want %+v", c.src, got, c.want)
		}
	}
}

func TestStringRoundTrip(t *testing.T) {
	in := newVM(t, nil, nil, nil)

	for _, s := range []string{"ascii", "日本語 と emoji 🍣", "a\x00b", "\xff\xfe raw bytes"} {
		if err := in.ArgsReset(); err != nil {
			t.Fatal(err)
		}
		if err := in.PushString(s); err != nil {
			t.Fatal(err)
		}
		// String#+ hands the argument straight back through the boundary.
		empty := mustEval(t, in, "''", true)
		v, err := in.Call(empty.Ref, "+", false)
		if err != nil {
			t.Fatalf("String#+ %q: %v", s, err)
		}
		if v.Str != s {
			t.Errorf("round trip of %q gave %q", s, v.Str)
		}
		in.ReleaseRef(empty.Ref)
	}

	// Byte counts survive an embedded NUL: mruby sees three bytes, not a terminated string.
	if err := in.ArgsReset(); err != nil {
		t.Fatal(err)
	}
	if err := in.PushString("a\x00b"); err != nil {
		t.Fatal(err)
	}
	ary := mustEval(t, in, "[]", true)
	defer in.ReleaseRef(ary.Ref)
	if _, err := in.Call(ary.Ref, "push", false); err != nil {
		t.Fatal(err)
	}
	elem, err := in.ArrayGet(ary.Ref, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	defer in.ReleaseRef(elem.Ref)
	size, err := in.Call(elem.Ref, "bytesize", false)
	if err != nil {
		t.Fatal(err)
	}
	if size.Int != 3 {
		t.Errorf("bytesize of the pushed \"a\\0b\" = %d, want 3", size.Int)
	}

	// Characters, not bytes: the build defines MRB_UTF8_STRING.
	if chars := mustEval(t, in, "'日本語'.size", false); chars.Int != 3 {
		t.Errorf("'日本語'.size = %d, want 3", chars.Int)
	}
}

func TestRubyExceptionCarriesClassMessageBacktrace(t *testing.T) {
	in := newVM(t, nil, nil, nil)
	_, err := in.Eval("def boom; raise ArgumentError, 'bad thing'; end; boom", false)

	var re *RubyError
	if !errors.As(err, &re) {
		t.Fatalf("Eval error = %v (%T), want *RubyError", err, err)
	}
	if re.Class != "ArgumentError" {
		t.Errorf("Class = %q, want ArgumentError", re.Class)
	}
	if re.Message != "bad thing" {
		t.Errorf("Message = %q, want \"bad thing\"", re.Message)
	}
	if !strings.Contains(re.Backtrace, "boom") {
		t.Errorf("Backtrace = %q, want it to name the raising method", re.Backtrace)
	}

	ref, err := in.ErrorRef()
	if err != nil {
		t.Fatal(err)
	}
	if ref == 0 {
		t.Fatal("ErrorRef = 0, want the exception object")
	}
	defer in.ReleaseRef(ref)
	if k, err := in.RefKind(ref); err != nil || k != KindOther {
		t.Errorf("RefKind(exception) = %v, %v", k, err)
	}
	msg, err := in.Call(ref, "message", false)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Str != "bad thing" {
		t.Errorf("exception ref message = %q", msg.Str)
	}
}

func TestSyntaxError(t *testing.T) {
	in := newVM(t, nil, nil, nil)
	_, err := in.Eval("1 +", false)

	var re *RubyError
	if !errors.As(err, &re) {
		t.Fatalf("Eval error = %v (%T), want *RubyError", err, err)
	}
	if re.Class != "SyntaxError" {
		t.Errorf("Class = %q, want SyntaxError", re.Class)
	}
	// The VM survives a syntax error.
	if v := mustEval(t, in, "1 + 1", false); v.Int != 2 {
		t.Errorf("after a syntax error: 1 + 1 = %d", v.Int)
	}
}

func TestRefReleaseReusesSlots(t *testing.T) {
	in := newVM(t, nil, nil, nil)

	const n = 64
	refs := make([]int32, 0, n)
	for i := 0; i < n; i++ {
		v := mustEval(t, in, fmt.Sprintf("'ref %d'", i), true)
		if v.Ref == 0 {
			t.Fatal("Eval with wantRef gave ref 0")
		}
		refs = append(refs, v.Ref)
	}
	grown, err := in.RegistryLen()
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range refs {
		if k, err := in.RefKind(ref); err != nil || k != KindString {
			t.Fatalf("RefKind(%d) = %v, %v", ref, k, err)
		}
		in.ReleaseRef(ref)
	}
	// The queue is drained by the next call that takes the lock.
	if k, err := in.RefKind(refs[0]); err != nil || k != KindInvalid {
		t.Errorf("RefKind of a released ref = %v, %v; want KindInvalid", k, err)
	}
	for i := 0; i < n; i++ {
		v := mustEval(t, in, fmt.Sprintf("'again %d'", i), true)
		in.ReleaseRef(v.Ref)
	}
	after, err := in.RegistryLen()
	if err != nil {
		t.Fatal(err)
	}
	if after != grown {
		t.Errorf("registry grew from %d to %d over a second round of %d refs", grown, after, n)
	}
}

func TestArgsPushEveryKindAndCall(t *testing.T) {
	in := newVM(t, nil, nil, nil)

	// String#* takes the repeat count from the scratch.
	s := mustEval(t, in, "'ab'", true)
	defer in.ReleaseRef(s.Ref)
	if err := in.ArgsReset(); err != nil {
		t.Fatal(err)
	}
	if err := in.PushInt(3); err != nil {
		t.Fatal(err)
	}
	v, err := in.Call(s.Ref, "*", false)
	if err != nil {
		t.Fatal(err)
	}
	if v.Str != "ababab" {
		t.Errorf("'ab' * 3 = %q", v.Str)
	}

	// Array#push takes one of every pushable kind.
	ary := mustEval(t, in, "[]", true)
	defer in.ReleaseRef(ary.Ref)
	held := mustEval(t, in, "Object.new", true)
	defer in.ReleaseRef(held.Ref)

	if err := in.ArgsReset(); err != nil {
		t.Fatal(err)
	}
	for _, push := range []func() error{
		in.PushNil,
		func() error { return in.PushBool(true) },
		func() error { return in.PushBool(false) },
		func() error { return in.PushInt(math.MinInt64) },
		func() error { return in.PushFloat(2.25) },
		func() error { return in.PushString("str") },
		func() error { return in.PushSymbol("sym") },
		func() error { return in.PushRef(held.Ref) },
	} {
		if err := push(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := in.Call(ary.Ref, "push", false); err != nil {
		t.Fatal(err)
	}

	n, err := in.ArrayLen(ary.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if n != 8 {
		t.Fatalf("Array#push of 8 arguments left %d elements", n)
	}
	want := []Value{
		{Kind: KindNil},
		{Kind: KindTrue},
		{Kind: KindFalse},
		{Kind: KindInt, Int: math.MinInt64},
		{Kind: KindFloat, Float: 2.25},
		{Kind: KindString, Str: "str"},
		{Kind: KindSymbol, Str: "sym"},
		{Kind: KindOther},
	}
	for i, w := range want {
		got, err := in.ArrayGet(ary.Ref, int64(i), false)
		if err != nil {
			t.Fatal(err)
		}
		if got != w {
			t.Errorf("element %d = %+v, want %+v", i, got, w)
		}
	}

	// The pushed ref is the same object, not a copy.
	same, err := in.ArrayGet(ary.Ref, 7, true)
	if err != nil {
		t.Fatal(err)
	}
	defer in.ReleaseRef(same.Ref)
	if err := in.ArgsReset(); err != nil {
		t.Fatal(err)
	}
	if err := in.PushRef(held.Ref); err != nil {
		t.Fatal(err)
	}
	eq, err := in.Call(same.Ref, "equal?", false)
	if err != nil {
		t.Fatal(err)
	}
	if eq.Kind != KindTrue {
		t.Errorf("the pushed object came back as a different object (%v)", eq.Kind)
	}
}

func TestObjectIDComesFromTheInterpreterNotFromRuby(t *testing.T) {
	in := newVM(t, nil, nil, nil)

	mustEval(t, in, "$held = 'the same object'", false)
	first, err := in.ResultRef()
	if err != nil {
		t.Fatal(err)
	}
	defer in.ReleaseRef(first)
	second := mustEval(t, in, "$held", true).Ref
	defer in.ReleaseRef(second)
	other := mustEval(t, in, "'another object'", true).Ref
	defer in.ReleaseRef(other)

	id, err := in.ObjectID(first)
	if err != nil {
		t.Fatal(err)
	}
	if id == 0 {
		t.Fatal("ObjectID of a held value = 0")
	}
	if again, err := in.ObjectID(second); err != nil || again != id {
		t.Errorf("two refs to one object have ids %d and %d (%v)", id, again, err)
	}
	if elsewhere, err := in.ObjectID(other); err != nil || elsewhere == id {
		t.Errorf("two objects share the id %d (%v)", elsewhere, err)
	}
	if unknown, err := in.ObjectID(9999); err != nil || unknown != 0 {
		t.Errorf("ObjectID of an unknown ref = %d, %v; want 0", unknown, err)
	}

	// A Ruby-level override answers what it likes; the identity the host reads is not it.
	overridden := mustEval(t, in, "class Sneaky; def object_id; 1; end; def __id__; 1; end; end; Sneaky.new", true).Ref
	defer in.ReleaseRef(overridden)
	id, err = in.ObjectID(overridden)
	if err != nil {
		t.Fatal(err)
	}
	if id == 1 {
		t.Error("ObjectID answered what the overridden object_id answers")
	}
	if v := mustEval(t, in, "Sneaky.new.object_id", false); v.Int != 1 {
		t.Errorf("the override is not in effect on the Ruby side: %+v", v)
	}
}

func TestCaptureArgCrossesBetweenImmediateAndRef(t *testing.T) {
	in := newVM(t, nil, nil, nil)

	// An immediate becomes a ref of its own.
	if err := in.ArgsReset(); err != nil {
		t.Fatal(err)
	}
	if err := in.PushString("text"); err != nil {
		t.Fatal(err)
	}
	v, err := in.CaptureArg(true)
	if err != nil {
		t.Fatal(err)
	}
	defer in.ReleaseRef(v.Ref)
	if v.Kind != KindString || v.Str != "text" || v.Ref == 0 {
		t.Fatalf("CaptureArg of a String = %+v", v)
	}
	size, err := in.Call(v.Ref, "size", false)
	if err != nil {
		t.Fatal(err)
	}
	if size.Int != 4 {
		t.Errorf("the captured ref is not the String that was pushed: %+v", size)
	}

	// A ref reads back as the immediate it holds.
	if err := in.ArgsReset(); err != nil {
		t.Fatal(err)
	}
	if err := in.PushRef(v.Ref); err != nil {
		t.Fatal(err)
	}
	back, err := in.CaptureArg(false)
	if err != nil {
		t.Fatal(err)
	}
	if back.Kind != KindString || back.Str != "text" {
		t.Errorf("CaptureArg of a ref = %+v", back)
	}

	// The scratch is emptied, so the next one has nothing to capture.
	if _, err := in.CaptureArg(false); err == nil {
		t.Error("CaptureArg of an empty scratch succeeded")
	}
	if err := in.ArgsReset(); err != nil {
		t.Fatal(err)
	}
	if err := in.PushInt(1); err != nil {
		t.Fatal(err)
	}
	if err := in.PushInt(2); err != nil {
		t.Fatal(err)
	}
	if _, err := in.CaptureArg(false); err == nil {
		t.Error("CaptureArg of two values succeeded")
	}
}

func TestNewArrayAndNewHashFromTheScratch(t *testing.T) {
	in := newVM(t, nil, nil, nil)

	held := mustEval(t, in, "Object.new", true)
	defer in.ReleaseRef(held.Ref)
	if err := in.ArgsReset(); err != nil {
		t.Fatal(err)
	}
	for _, push := range []func() error{
		func() error { return in.PushInt(1) },
		func() error { return in.PushString("two") },
		func() error { return in.PushRef(held.Ref) },
	} {
		if err := push(); err != nil {
			t.Fatal(err)
		}
	}
	ary, err := in.NewArray(true)
	if err != nil {
		t.Fatal(err)
	}
	defer in.ReleaseRef(ary.Ref)
	if ary.Kind != KindArray || ary.Ref == 0 {
		t.Fatalf("NewArray = %+v", ary)
	}
	if n, err := in.ArrayLen(ary.Ref); err != nil || n != 3 {
		t.Fatalf("the new Array holds %d elements (%v)", n, err)
	}
	if err := in.ArgsReset(); err != nil {
		t.Fatal(err)
	}
	if s, err := in.Call(ary.Ref, "inspect", false); err != nil || s.Str != `[1, "two", `+objectInspect(t, in, held.Ref)+"]" {
		t.Errorf("the new Array inspects as %q (%v)", s.Str, err)
	}

	// The scratch is consumed, so the next Array is empty.
	empty, err := in.NewArray(true)
	if err != nil {
		t.Fatal(err)
	}
	defer in.ReleaseRef(empty.Ref)
	if n, err := in.ArrayLen(empty.Ref); err != nil || n != 0 {
		t.Errorf("the second Array holds %d elements (%v)", n, err)
	}

	if err := in.ArgsReset(); err != nil {
		t.Fatal(err)
	}
	if err := in.PushSymbol("key"); err != nil {
		t.Fatal(err)
	}
	if err := in.PushInt(7); err != nil {
		t.Fatal(err)
	}
	if err := in.PushString("other"); err != nil {
		t.Fatal(err)
	}
	if err := in.PushFloat(0.5); err != nil {
		t.Fatal(err)
	}
	hash, err := in.NewHash(true)
	if err != nil {
		t.Fatal(err)
	}
	defer in.ReleaseRef(hash.Ref)
	if hash.Kind != KindHash || hash.Ref == 0 {
		t.Fatalf("NewHash = %+v", hash)
	}
	if err := in.ArgsReset(); err != nil {
		t.Fatal(err)
	}
	if s, err := in.Call(hash.Ref, "inspect", false); err != nil || s.Str != `{key: 7, "other" => 0.5}` {
		t.Errorf("the new Hash inspects as %q (%v)", s.Str, err)
	}

	// A key with no value is a misuse the guest refuses.
	if err := in.ArgsReset(); err != nil {
		t.Fatal(err)
	}
	if err := in.PushSymbol("odd"); err != nil {
		t.Fatal(err)
	}
	if _, err := in.NewHash(false); err == nil {
		t.Error("NewHash of an odd number of values succeeded")
	}
	// The VM still works.
	if v := mustEval(t, in, "'fine'", false); v.Str != "fine" {
		t.Errorf("after the refused NewHash: %+v", v)
	}
}

// objectInspect is what Ruby's inspect says about a plain object, whose address is in the text.
func objectInspect(t *testing.T, in *Instance, ref int32) string {
	t.Helper()
	if err := in.ArgsReset(); err != nil {
		t.Fatal(err)
	}
	v, err := in.Call(ref, "inspect", false)
	if err != nil {
		t.Fatal(err)
	}
	return v.Str
}

func TestYieldProc(t *testing.T) {
	in := newVM(t, nil, nil, nil)
	p := mustEval(t, in, "->(a, b) { a * b }", true)
	defer in.ReleaseRef(p.Ref)
	if k, err := in.RefKind(p.Ref); err != nil || k != KindOther {
		t.Fatalf("RefKind(proc) = %v, %v", k, err)
	}

	if err := in.ArgsReset(); err != nil {
		t.Fatal(err)
	}
	if err := in.PushInt(6); err != nil {
		t.Fatal(err)
	}
	if err := in.PushInt(7); err != nil {
		t.Fatal(err)
	}
	v, err := in.Yield(p.Ref, false)
	if err != nil {
		t.Fatal(err)
	}
	if v.Int != 42 {
		t.Errorf("proc call = %d, want 42", v.Int)
	}

	// A Proc passed as a method's block.
	doubler := mustEval(t, in, "->(x) { x * 2 }", true)
	defer in.ReleaseRef(doubler.Ref)
	ary := mustEval(t, in, "[1, 2, 3]", true)
	defer in.ReleaseRef(ary.Ref)
	mapped, err := in.CallBlock(ary.Ref, "map", doubler.Ref, true)
	if err != nil {
		t.Fatal(err)
	}
	defer in.ReleaseRef(mapped.Ref)
	if err := in.ArgsReset(); err != nil {
		t.Fatal(err)
	}
	joined, err := in.Call(mapped.Ref, "inspect", false)
	if err != nil {
		t.Fatal(err)
	}
	if joined.Str != "[2, 4, 6]" {
		t.Errorf("map with a block ref = %s", joined.Str)
	}
}

func TestArrayAndHashTraversal(t *testing.T) {
	in := newVM(t, nil, nil, nil)

	ary := mustEval(t, in, "[1, 'two', :three, [4]]", true)
	defer in.ReleaseRef(ary.Ref)
	n, err := in.ArrayLen(ary.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Fatalf("ArrayLen = %d, want 4", n)
	}
	kinds := []Kind{KindInt, KindString, KindSymbol, KindArray}
	for i, want := range kinds {
		v, err := in.ArrayGet(ary.Ref, int64(i), false)
		if err != nil {
			t.Fatal(err)
		}
		if v.Kind != want {
			t.Errorf("element %d is %v, want %v", i, v.Kind, want)
		}
	}
	if _, err := in.ArrayLen(ary.Ref + 1000); err == nil {
		// An unknown ref answers -1 rather than failing, which is what the ABI promises.
		if n, _ := in.ArrayLen(ary.Ref + 1000); n != -1 {
			t.Errorf("ArrayLen of an unknown ref = %d, want -1", n)
		}
	}

	hash := mustEval(t, in, "{'a' => 1, :b => 2.5, 3 => 'c'}", true)
	defer in.ReleaseRef(hash.Ref)
	keys, err := in.HashKeys(hash.Ref)
	if err != nil {
		t.Fatal(err)
	}
	defer in.ReleaseRef(keys)
	count, err := in.ArrayLen(keys)
	if err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("HashKeys gave %d keys, want 3", count)
	}
	seen := map[string]Value{}
	for i := int64(0); i < count; i++ {
		key, err := in.ArrayGet(keys, i, true)
		if err != nil {
			t.Fatal(err)
		}
		val, err := in.HashGet(hash.Ref, key.Ref, false)
		if err != nil {
			t.Fatal(err)
		}
		in.ReleaseRef(key.Ref)
		switch key.Kind {
		case KindString, KindSymbol:
			seen[key.Str] = val
		case KindInt:
			seen[fmt.Sprint(key.Int)] = val
		}
	}
	if v, ok := seen["a"]; !ok || v.Int != 1 {
		t.Errorf("hash['a'] = %+v", v)
	}
	if v, ok := seen["b"]; !ok || v.Float != 2.5 {
		t.Errorf("hash[:b] = %+v", v)
	}
	if v, ok := seen["3"]; !ok || v.Str != "c" {
		t.Errorf("hash[3] = %+v", v)
	}
}

func TestDefineClassAndMethodOnIt(t *testing.T) {
	table := hostTable{}
	in := newVM(t, nil, nil, table)

	cls, err := in.DefineClass("Greeter", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer in.ReleaseRef(cls)

	table[7] = func(vm *Instance) int32 {
		// The receiver crosses as a ref, so the host can call back into it.
		self := vm.HostSelfRef()
		defer vm.ReleaseRef(self)
		who, err := vm.Call(self, "name", false)
		if err != nil {
			t.Error(err)
			return 1
		}
		if k := vm.HostArgKind(0); k != KindString {
			vm.HostRaise("TypeError", fmt.Sprintf("argument 0 is %v", k))
			return 1
		}
		// A non-immediate argument crosses as a ref too.
		extra := vm.HostArgRef(1)
		defer vm.ReleaseRef(extra)
		n, err := vm.ArrayLen(extra)
		if err != nil {
			t.Error(err)
			return 1
		}
		if err := vm.ArgsReset(); err != nil {
			t.Error(err)
		}
		if err := vm.PushString(fmt.Sprintf("%s says hello, %s (+%d)", who.Str, vm.HostArgString(0), n)); err != nil {
			t.Error(err)
		}
		return 0
	}
	if err := in.DefineMethod(cls, "greet", 7); err != nil {
		t.Fatal(err)
	}
	mustEval(t, in, "class Greeter; def initialize(n); @n = n; end; def name; @n; end; end", false)

	v := mustEval(t, in, "Greeter.new('G').greet('world', [1, 2])", false)
	if v.Str != "G says hello, world (+2)" {
		t.Errorf("greet = %q", v.Str)
	}
	if _, err := in.Eval("Greeter.new('G').greet(1, [])", false); err == nil {
		t.Fatal("greet(1, []) succeeded, want a TypeError")
	} else {
		var re *RubyError
		if !errors.As(err, &re) || re.Class != "TypeError" {
			t.Errorf("greet(1, []) error = %v", err)
		}
	}
}

func TestKernelMethodFromRuby(t *testing.T) {
	table := hostTable{}
	in := newVM(t, nil, nil, table)

	const (
		fnAdd = 1
		fnCat = 2
		fnBad = 3
	)
	table[fnAdd] = func(vm *Instance) int32 {
		if n := vm.HostArgsCount(); n != 2 {
			vm.HostRaise("ArgumentError", fmt.Sprintf("wrong number of arguments (%d for 2)", n))
			return 1
		}
		sum := vm.HostArgInt(0) + vm.HostArgInt(1)
		_ = vm.ArgsReset()
		_ = vm.PushInt(sum)
		return 0
	}
	table[fnCat] = func(vm *Instance) int32 {
		_ = vm.ArgsReset()
		_ = vm.PushString(vm.HostArgString(0) + "!" + fmt.Sprint(vm.HostArgFloat(1)))
		return 0
	}
	table[fnBad] = func(vm *Instance) int32 {
		vm.HostRaise("ArgumentError", "host said no")
		return 1
	}
	for name, id := range map[string]int32{"go_add": fnAdd, "go_cat": fnCat, "go_bad": fnBad} {
		if err := in.DefineMethod(0, name, id); err != nil {
			t.Fatal(err)
		}
	}

	if v := mustEval(t, in, "go_add(20, 22)", false); v.Int != 42 {
		t.Errorf("go_add = %+v", v)
	}
	if v := mustEval(t, in, "go_cat('x', 0.5)", false); v.Str != "x!0.5" {
		t.Errorf("go_cat = %+v", v)
	}
	if v := mustEval(t, in, "begin; go_add(1); rescue => e; e.class.to_s; end", false); v.Str != "ArgumentError" {
		t.Errorf("go_add with one argument = %+v", v)
	}

	_, err := in.Eval("go_bad", false)
	var re *RubyError
	if !errors.As(err, &re) {
		t.Fatalf("go_bad error = %v (%T), want *RubyError", err, err)
	}
	if re.Class != "ArgumentError" || re.Message != "host said no" {
		t.Errorf("go_bad raised %s: %s", re.Class, re.Message)
	}
	// A raise from the host is an ordinary Ruby exception on the Ruby side too.
	if v := mustEval(t, in, "begin; go_bad; rescue => e; e.message; end", false); v.Str != "host said no" {
		t.Errorf("rescued host raise = %+v", v)
	}
}

// Ruby calls a Go method, which yields back into the block it was given, and that block calls a second Go method.
func TestHostCallNesting(t *testing.T) {
	table := hostTable{}
	in := newVM(t, nil, nil, table)

	const (
		fnOuter = 10
		fnInner = 11
	)
	var innerCalls int
	table[fnOuter] = func(vm *Instance) int32 {
		block := vm.HostBlockRef()
		if block == 0 {
			vm.HostRaise("LocalJumpError", "no block given")
			return 1
		}
		defer vm.ReleaseRef(block)
		seed := vm.HostArgInt(0)

		if err := vm.ArgsReset(); err != nil {
			t.Error(err)
		}
		if err := vm.PushInt(seed + 1); err != nil {
			t.Error(err)
		}
		v, err := vm.Yield(block, false)
		if err != nil {
			t.Errorf("nested Yield: %v", err)
			vm.HostRaise("RuntimeError", err.Error())
			return 1
		}
		_ = vm.ArgsReset()
		_ = vm.PushInt(v.Int * 10)
		return 0
	}
	table[fnInner] = func(vm *Instance) int32 {
		innerCalls++
		// Reaching the guest again from the innermost callback: the scratch and host-argument stacks must both survive it.
		doubled, err := vm.Eval("->(x) { x * 2 }", true)
		if err != nil {
			t.Errorf("nested Eval: %v", err)
			return 1
		}
		defer vm.ReleaseRef(doubled.Ref)
		_ = vm.ArgsReset()
		_ = vm.PushInt(vm.HostArgInt(0))
		v, err := vm.Yield(doubled.Ref, false)
		if err != nil {
			t.Errorf("nested Yield: %v", err)
			return 1
		}
		_ = vm.ArgsReset()
		_ = vm.PushInt(v.Int + 3)
		return 0
	}
	if err := in.DefineMethod(0, "go_outer", fnOuter); err != nil {
		t.Fatal(err)
	}
	if err := in.DefineMethod(0, "go_inner", fnInner); err != nil {
		t.Fatal(err)
	}

	// go_outer(4) yields 5 to the block, the block calls go_inner(5) = 5*2+3 = 13, and go_outer returns 130.
	v := mustEval(t, in, "go_outer(4) { |x| go_inner(x) }", false)
	if v.Int != 130 {
		t.Errorf("nested call chain = %d, want 130", v.Int)
	}
	if innerCalls != 1 {
		t.Errorf("go_inner ran %d times, want 1", innerCalls)
	}

	// The argument scratch is intact afterwards.
	if v := mustEval(t, in, "go_outer(0) { |x| go_inner(x) }", false); v.Int != 50 {
		t.Errorf("second nested call chain = %d, want 50", v.Int)
	}
}

func TestInstancesAreIndependent(t *testing.T) {
	var outA, outB bytes.Buffer
	a := newVM(t, &outA, &outA, nil)
	b := newVM(t, &outB, &outB, nil)

	mustEval(t, a, "$state = 'a'", false)
	mustEval(t, b, "$state = 'b'", false)
	for i := 0; i < 3; i++ {
		if v := mustEval(t, a, "$state", false); v.Str != "a" {
			t.Fatalf("instance a sees %q", v.Str)
		}
		if v := mustEval(t, b, "$state", false); v.Str != "b" {
			t.Fatalf("instance b sees %q", v.Str)
		}
		mustEval(t, a, fmt.Sprintf("$n = %d", i), false)
		mustEval(t, b, "$n = 100", false)
	}
	if v := mustEval(t, a, "$n", false); v.Int != 2 {
		t.Errorf("instance a $n = %d, want 2", v.Int)
	}
	if v := mustEval(t, b, "$n", false); v.Int != 100 {
		t.Errorf("instance b $n = %d, want 100", v.Int)
	}

	mustEval(t, a, "puts 'from a'", false)
	if outA.String() != "from a\n" {
		t.Errorf("instance a stdout = %q", outA.String())
	}
	if outB.Len() != 0 {
		t.Errorf("instance b stdout = %q, want nothing", outB.String())
	}
}

func TestStdoutAndStderrCapture(t *testing.T) {
	var out, errOut bytes.Buffer
	in := newVM(t, &out, &errOut, nil)

	mustEval(t, in, "print 'no newline'", false)
	mustEval(t, in, "puts 'and a line'", false)
	mustEval(t, in, "puts [1, 2]", false)
	if got, want := out.String(), "no newlineand a line\n1\n2\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
	if errOut.Len() != 0 {
		t.Errorf("stderr = %q, want nothing", errOut.String())
	}
}

func TestProcExitBecomesAnError(t *testing.T) {
	in := newVM(t, nil, nil, nil)
	_, err := in.Eval("exit! 3", false)

	var ee *ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("Eval(exit!) = %v (%T), want *ExitError", err, err)
	}
	if ee.Code != 3 {
		t.Errorf("exit code = %d, want 3", ee.Code)
	}
}

func TestResultToS(t *testing.T) {
	in := newVM(t, nil, nil, nil)
	if v := mustEval(t, in, "[1, :two]", false); v.Str != "" {
		t.Errorf("a non-string result carries a string: %q", v.Str)
	}
	s, err := in.ResultToS()
	if err != nil {
		t.Fatal(err)
	}
	if s != "[1, :two]" {
		t.Errorf("ResultToS = %q", s)
	}

	mustEval(t, in, "class Boom; def to_s; raise 'no to_s'; end; end; Boom.new", false)
	if _, err := in.ResultToS(); err == nil {
		t.Error("ResultToS of a raising to_s succeeded")
	}
}

func TestResultRefAfterTheCall(t *testing.T) {
	in := newVM(t, nil, nil, nil)

	v := mustEval(t, in, "[1, 2, 3]", false)
	if v.Ref != 0 {
		t.Fatalf("Eval without wantRef gave ref %d", v.Ref)
	}
	ref, err := in.ResultRef()
	if err != nil {
		t.Fatal(err)
	}
	if ref == 0 {
		t.Fatal("ResultRef = 0")
	}
	defer in.ReleaseRef(ref)
	if n, err := in.ArrayLen(ref); err != nil || n != 3 {
		t.Fatalf("ArrayLen of the deferred ref = %d, %v", n, err)
	}

	// A second one is another ref to the same object, not to a copy.
	again, err := in.ResultRef()
	if err != nil {
		t.Fatal(err)
	}
	defer in.ReleaseRef(again)
	if err := in.ArgsReset(); err != nil {
		t.Fatal(err)
	}
	if err := in.PushRef(ref); err != nil {
		t.Fatal(err)
	}
	eq, err := in.Call(again, "equal?", false)
	if err != nil {
		t.Fatal(err)
	}
	if eq.Kind != KindTrue {
		t.Errorf("two refs to one result are not the same object (%v)", eq.Kind)
	}

	// The capture is what it reads, so a later call moves it on.
	mustEval(t, in, "'other'", false)
	moved, err := in.ResultRef()
	if err != nil {
		t.Fatal(err)
	}
	defer in.ReleaseRef(moved)
	if k, err := in.RefKind(moved); err != nil || k != KindString {
		t.Errorf("RefKind after a second call = %v, %v", k, err)
	}
}

func TestGuestErrorNamesTheGuestPanics(t *testing.T) {
	for _, c := range []struct {
		panicked any
		want     string
	}{
		{&rtExit{code: 3}, "mrubyvm: the guest exited with status 3"},
		{&rtTrap{msg: "unreachable"}, "mrubyvm: wasm trap: unreachable"},
		{&rtLinkError{msg: "no such import"}, "mrubyvm: no such import"},
	} {
		err := GuestError(c.panicked)
		if err == nil {
			t.Errorf("GuestError(%T) = nil", c.panicked)
			continue
		}
		if err.Error() != c.want {
			t.Errorf("GuestError(%T) = %q, want %q", c.panicked, err, c.want)
		}
	}
	for _, other := range []any{"a string", fmt.Errorf("an error"), 42} {
		if err := GuestError(other); err != nil {
			t.Errorf("GuestError(%T) = %v, want nil: it did not come from the guest", other, err)
		}
	}
}

func TestABIErrorsOnBadRefs(t *testing.T) {
	in := newVM(t, nil, nil, nil)

	_, err := in.Call(9999, "to_s", false)
	var ae *ABIError
	if !errors.As(err, &ae) {
		t.Fatalf("Call on an unknown ref = %v (%T), want *ABIError", err, err)
	}
	if !strings.Contains(ae.Message, "receiver") {
		t.Errorf("ABIError message = %q", ae.Message)
	}
	if _, err := in.Yield(9999, false); err == nil {
		t.Error("Yield on an unknown ref succeeded")
	}
	notAProc := mustEval(t, in, "42", true)
	defer in.ReleaseRef(notAProc.Ref)
	if _, err := in.Yield(notAProc.Ref, false); err == nil {
		t.Error("Yield on an Integer succeeded")
	}
	if _, err := in.HashKeys(notAProc.Ref); err == nil {
		t.Error("HashKeys on an Integer succeeded")
	}
	// The VM still works.
	if v := mustEval(t, in, "'fine'", false); v.Str != "fine" {
		t.Errorf("after ABI errors: %+v", v)
	}
}

func TestModuleFunctionDefinition(t *testing.T) {
	table := hostTable{}
	in := newVM(t, nil, nil, table)

	table[42] = func(vm *Instance) int32 {
		_ = vm.ArgsReset()
		_ = vm.PushSymbol("from-go")
		return 0
	}
	cls, err := in.DefineClass("Util", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer in.ReleaseRef(cls)
	if err := in.DefineModuleFunction(cls, "tag", 42); err != nil {
		t.Fatal(err)
	}
	if v := mustEval(t, in, "Util.tag", false); v.Kind != KindSymbol || v.Str != "from-go" {
		t.Errorf("Util.tag = %+v", v)
	}
	if v := mustEval(t, in, "Util.new.tag", false); v.Str != "from-go" {
		t.Errorf("Util.new.tag = %+v", v)
	}
}

func TestMissingExportFailsLoud(t *testing.T) {
	in := newVM(t, nil, nil, nil)
	saved := in.mod.Exports["dm_eval"]
	delete(in.mod.Exports, "dm_eval")
	defer func() { in.mod.Exports["dm_eval"] = saved }()

	if _, err := bindFuncs(in.mod); err == nil || !strings.Contains(err.Error(), "dm_eval") {
		t.Errorf("bindFuncs with a missing export gave %v", err)
	}

	in.mod.Exports["dm_eval"] = func() {}
	if _, err := bindFuncs(in.mod); err == nil || !strings.Contains(err.Error(), "dm_eval") {
		t.Errorf("bindFuncs with a mistyped export gave %v", err)
	}
}

// The registry is what keeps a host-held value alive: the collector must not take one while Ruby churns through garbage.
func TestHeldRefsSurviveGC(t *testing.T) {
	in := newVM(t, nil, nil, nil)

	held := mustEval(t, in, "'pinned' * 10", true)
	defer in.ReleaseRef(held.Ref)
	hash := mustEval(t, in, "{key: 'value'}", true)
	defer in.ReleaseRef(hash.Ref)

	mustEval(t, in, "2000.times { |i| ('garbage' * (i % 17)).upcase.split('A') }; GC.start", false)

	size, err := in.Call(held.Ref, "size", false)
	if err != nil {
		t.Fatal(err)
	}
	if size.Int != 60 {
		t.Errorf("the held String is %d characters after a GC, want 60", size.Int)
	}
	keys, err := in.HashKeys(hash.Ref)
	if err != nil {
		t.Fatal(err)
	}
	defer in.ReleaseRef(keys)
	key, err := in.ArrayGet(keys, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if key.Kind != KindSymbol || key.Str != "key" {
		t.Errorf("the held Hash lost its key after a GC: %+v", key)
	}
}
