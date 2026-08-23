package mruby

import (
	"errors"
	"strings"
	"testing"
)

// armed installs fn on vm and disarms it when the test ends, so a later assertion runs without the hook interrupting it.
func armed(t *testing.T, vm *VM, every uint64, fn func() error) {
	t.Helper()
	if err := vm.SetHook(every, fn); err != nil {
		t.Fatalf("SetHook: %v", err)
	}
	t.Cleanup(func() {
		if err := vm.SetHook(0, nil); err != nil {
			t.Errorf("disarming: %v", err)
		}
	})
}

// interrupt is the error an interrupting callback keeps returning: an Exception-derived class, which no bare rescue catches.
func interrupt() error {
	return Raise("Interrupt", "the host stopped this script")
}

func defineInterrupt(t *testing.T, vm *VM) {
	t.Helper()
	if _, err := vm.Eval("class Interrupt < Exception; end"); err != nil {
		t.Fatalf("defining Interrupt: %v", err)
	}
}

func wantInterrupt(t *testing.T, err error) {
	t.Helper()
	var re *RubyError
	if !errors.As(err, &re) {
		t.Fatalf("the interrupted evaluation gave %v (%T), want *RubyError", err, err)
	}
	if re.Class != "Interrupt" {
		t.Fatalf("the interrupted evaluation raised %s: %s", re.Class, re.Message)
	}
}

func TestHookInterruptsAnEndlessLoop(t *testing.T) {
	vm := newVM(t)
	defineInterrupt(t, vm)
	armed(t, vm, 1000, interrupt)

	_, err := vm.Eval("loop { }")
	wantInterrupt(t, err)

	if err := vm.SetHook(0, nil); err != nil {
		t.Fatal(err)
	}
	if n, _ := mustEval(t, vm, "6 * 7").AsInt(); n != 42 {
		t.Error("the VM does not compute after an interruption")
	}
}

// A bare rescue catches StandardError, and the interruption is not one: that is what keeps a script from swallowing it.
func TestHookInterruptIsNotSwallowedByABareRescue(t *testing.T) {
	vm := newVM(t)
	defineInterrupt(t, vm)
	armed(t, vm, 1000, interrupt)

	_, err := vm.Eval("begin; loop { }; rescue => e; e; end")
	wantInterrupt(t, err)
}

// The callback keeps firing while the raise it made unwinds, so it keeps returning its error until execution is back in Go.
func TestHookInterruptRunsEnsureBlocks(t *testing.T) {
	vm := newVM(t)
	defineInterrupt(t, vm)
	armed(t, vm, 1000, interrupt)

	_, err := vm.Eval("$ran = false; begin; loop { }; ensure; $ran = true; end")
	wantInterrupt(t, err)

	if err := vm.SetHook(0, nil); err != nil {
		t.Fatal(err)
	}
	ran, err := vm.GlobalVar("$ran")
	if err != nil {
		t.Fatal(err)
	}
	if b, err := ran.AsBool(); err != nil || !b {
		t.Errorf("$ran = %v (%v), want the ensure block to have run during the unwind", ran, err)
	}
}

// Fuel is the canonical use: a budget of instructions, and an error once it is spent.
func TestHookFuelBudget(t *testing.T) {
	vm := newVM(t)
	defineInterrupt(t, vm)

	fuel := 20
	armed(t, vm, 1000, func() error {
		if fuel > 0 {
			fuel--
			return nil
		}
		return interrupt()
	})

	_, err := vm.Eval("n = 0; loop { n += 1 }")
	wantInterrupt(t, err)
	if fuel != 0 {
		t.Errorf("the script stopped with %d fuel left", fuel)
	}

	if err := vm.SetHook(0, nil); err != nil {
		t.Fatal(err)
	}
	if n, _ := mustEval(t, vm, "6 * 7").AsInt(); n != 42 {
		t.Error("the VM does not compute after the fuel ran out")
	}
}

func TestHookPanicBecomesARubyError(t *testing.T) {
	vm := newVM(t)
	armed(t, vm, 1000, func() error {
		panic("the hook fell over")
	})

	_, err := vm.Eval("loop { }")
	var re *RubyError
	if !errors.As(err, &re) {
		t.Fatalf("a panicking hook gave %v (%T), want *RubyError", err, err)
	}
	if re.Class != "RuntimeError" {
		t.Errorf("a panicking hook raised %s, want RuntimeError", re.Class)
	}
	if !strings.Contains(re.Message, "the hook fell over") {
		t.Errorf("the message does not carry the panic: %q", re.Message)
	}

	if err := vm.SetHook(0, nil); err != nil {
		t.Fatal(err)
	}
	if n, _ := mustEval(t, vm, "6 * 7").AsInt(); n != 42 {
		t.Error("the VM does not compute after a panicking hook")
	}
}

// The callback runs on the goroutine holding the VM, so reading the VM from it is reentrancy, not a deadlock.
func TestHookReadsVMState(t *testing.T) {
	vm := newVM(t)
	if err := vm.SetGlobalVar("$probe", 7); err != nil {
		t.Fatal(err)
	}

	seen := int64(0)
	armed(t, vm, 500, func() error {
		v, err := vm.GlobalVar("$probe")
		if err != nil {
			return err
		}
		n, err := v.AsInt()
		if err != nil {
			return err
		}
		seen = n
		return nil
	})

	if n, _ := mustEval(t, vm, "n = 0; 500.times { |i| n += i }; n").AsInt(); n != 124750 {
		t.Fatalf("the computation under a reading hook gave %d", n)
	}
	if seen != 7 {
		t.Errorf("the hook read $probe as %d, want 7", seen)
	}
}

func TestSetHookValidation(t *testing.T) {
	vm := newVM(t)

	if err := vm.SetHook(0, func() error { return nil }); err == nil {
		t.Error("SetHook with an interval of 0 and a callback succeeded")
	}
	// A nil callback uninstalls whatever the interval says.
	if err := vm.SetHook(0, nil); err != nil {
		t.Errorf("SetHook(0, nil) = %v", err)
	}

	first, second := 0, 0
	armed(t, vm, 200, func() error {
		first++
		return nil
	})
	if err := vm.SetHook(200, func() error {
		second++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := vm.Eval("n = 0; 500.times { |i| n += i }; n"); err != nil {
		t.Fatal(err)
	}
	if second == 0 {
		t.Error("the second callback never ran")
	}
	if first != 0 {
		t.Errorf("the callback SetHook replaced ran %d times", first)
	}
}
