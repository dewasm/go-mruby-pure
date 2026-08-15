package mruby

import "testing"

func BenchmarkNew(b *testing.B) {
	for i := 0; i < b.N; i++ {
		if _, err := New(WithStdout(nil), WithStderr(nil)); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEvalArithmetic(b *testing.B) {
	vm := newVM(b, WithStdout(nil), WithStderr(nil))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := vm.Eval("1 + 2 * 3"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCallGoMethod(b *testing.B) {
	vm := newVM(b, WithStdout(nil), WithStderr(nil))
	if err := vm.Define("go_add", func(a, c int64) int64 { return a + c }); err != nil {
		b.Fatal(err)
	}
	if _, err := vm.Eval("def bench; go_add(1, 2); end"); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := vm.Call(nil, "bench"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkFib20(b *testing.B) {
	vm := newVM(b, WithStdout(nil), WithStderr(nil))
	if _, err := vm.Eval("def fib(n); n < 2 ? n : fib(n - 1) + fib(n - 2); end"); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		v, err := vm.Call(nil, "fib", 20)
		if err != nil {
			b.Fatal(err)
		}
		if n, _ := v.AsInt(); n != 6765 {
			b.Fatalf("fib(20) = %d", n)
		}
	}
}
