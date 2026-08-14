package mruby

import (
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"
)

func registryLen(t *testing.T, vm *VM) int32 {
	t.Helper()
	n, err := vm.in.RegistryLen()
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// collect runs the collector until the references the cleanups give back are back in the interpreter, or until it is clear they will not be.
func collect(t *testing.T, vm *VM, want int32) int32 {
	t.Helper()
	var got int32
	for i := 0; i < 200; i++ {
		runtime.GC()
		time.Sleep(time.Millisecond)
		// Reading the registry is a call, and a call is what takes the queue.
		got = registryLen(t, vm)
		if want != 0 && got <= want {
			break
		}
	}
	return got
}

func TestOneVMFromManyGoroutines(t *testing.T) {
	vm := newVM(t)
	if _, err := vm.Eval("def work(n); n * 3; end"); err != nil {
		t.Fatal(err)
	}

	const goroutines, rounds = 8, 40
	errs := make(chan error, goroutines*rounds)
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				v, err := vm.Call(nil, "work", i)
				if err != nil {
					errs <- err
					return
				}
				n, err := v.Int()
				if err != nil {
					errs <- err
					return
				}
				if n != int64(3*i) {
					errs <- fmt.Errorf("work(%d) = %d, want %d", i, n, 3*i)
					return
				}
				held, err := vm.Eval("[1, {a: 2}, 'three']")
				if err != nil {
					errs <- err
					return
				}
				if _, err := held.Export(); err != nil {
					errs <- err
					return
				}
				if held.String() == "" {
					errs <- fmt.Errorf("the Array has no to_s")
				}
				if i%2 == 0 {
					held.Release()
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// A host function runs on the goroutine that called into Ruby, and while it runs no other goroutine may use the VM, so goroutines that share one take turns.
func TestHostFunctionsFromManyGoroutines(t *testing.T) {
	vm := newVM(t)
	var count int
	mustDefine(t, vm, "go_count", func(n int64) int64 {
		count++
		return n * 2
	})
	if _, err := vm.Eval("def work(n); go_count(n) + [n].first; end"); err != nil {
		t.Fatal(err)
	}

	const goroutines, rounds = 8, 20
	var turn sync.Mutex
	errs := make(chan error, goroutines*rounds)
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				turn.Lock()
				v, err := vm.Call(nil, "work", i)
				turn.Unlock()
				if err != nil {
					errs <- err
					return
				}
				if n, _ := v.Int(); n != int64(3*i) {
					errs <- fmt.Errorf("work(%d) = %d, want %d", i, n, 3*i)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if count != goroutines*rounds {
		t.Errorf("the Go function ran %d times, want %d", count, goroutines*rounds)
	}
}

func TestManyVMsFromManyGoroutines(t *testing.T) {
	const goroutines = 6
	errs := make(chan error, goroutines)
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			vm, err := New(WithStdout(nil), WithStderr(nil))
			if err != nil {
				errs <- err
				return
			}
			if err := vm.Define("go_tag", func() int { return g }); err != nil {
				errs <- err
				return
			}
			for i := 0; i < 20; i++ {
				v, err := vm.Eval("$n = (($n || 0) + go_tag); [$n, go_tag]")
				if err != nil {
					errs <- err
					return
				}
				x, err := v.Export()
				if err != nil {
					errs <- err
					return
				}
				want := []any{int64(g * (i + 1)), int64(g)}
				if !equalAny(x, want) {
					errs <- fmt.Errorf("VM %d round %d = %#v, want %#v", g, i, x, want)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestReleasedValuesReuseTheirReferences(t *testing.T) {
	vm := newVM(t)
	const n = 300

	churn := func() {
		for i := 0; i < n; i++ {
			mustEval(t, vm, "[1, 2]").Release()
		}
	}
	churn()
	baseline := registryLen(t, vm)
	churn()
	if got := registryLen(t, vm); got != baseline {
		t.Errorf("the interpreter is holding %d references after %d released values, was %d", got, n, baseline)
	}
}

func TestCollectedValuesGiveTheirReferencesBack(t *testing.T) {
	vm := newVM(t)
	const n = 300

	churn := func() {
		for i := 0; i < n; i++ {
			if _, err := vm.Eval("[1, 2]"); err != nil {
				t.Fatal(err)
			}
		}
	}
	churn()
	baseline := collect(t, vm, 0)
	churn()
	if got := collect(t, vm, baseline); got > baseline {
		t.Errorf("the interpreter is holding %d references after %d collected values, was %d", got, n, baseline)
	}
}

// A Value made inside a host function is collectable there too: giving a reference back takes no lock, so it cannot wait on the call that is running.
func TestValuesCollectedInsideAHostFunction(t *testing.T) {
	vm := newVM(t)
	mustDefine(t, vm, "go_churn", func(c *Call) (any, error) {
		for i := 0; i < 50; i++ {
			v, err := c.VM().Eval("[1, 2]")
			if err != nil {
				return nil, err
			}
			if i%10 == 0 {
				runtime.GC()
				v.Release()
			}
		}
		runtime.GC()
		time.Sleep(time.Millisecond)
		return "done", nil
	})
	if got := mustEval(t, vm, "go_churn").String(); got != "done" {
		t.Errorf("go_churn = %q", got)
	}
	if v := mustEval(t, vm, "'still here'"); v.String() != "still here" {
		t.Error("the VM did not survive a collection inside a host function")
	}
}
