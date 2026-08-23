package mruby

import (
	"errors"
	"fmt"
	"math"

	"github.com/dewasm/go-mruby-pure/mrubyvm"
)

// SetHook arranges for fn to run every `every` VM instructions while Ruby code executes, which is what makes a runaway `loop {}` interruptible.
//
// A non-nil error from fn raises in Ruby at that instruction boundary the way a host function's error does: a *RubyError keeps its class, anything else is a RuntimeError.
// The raise surfaces from the running Eval or Call, and the VM stays usable afterwards.
//
// fn runs on the goroutine executing the VM while the VM lock is held, so it may call back into the VM under the reentrancy discipline of a host function: while it runs, no other goroutine may use this VM.
// It keeps firing during the unwind an earlier raise started, so a callback that means to interrupt should return its error until execution has come back to Go; returning nil there lets a `rescue` swallow the interruption and carry on.
// A C-level loop inside the interpreter and a blocking host function are not interrupted: the hook only runs between VM instructions.
//
// A nil fn uninstalls the hook whatever every says; otherwise every is at least 1.
func (vm *VM) SetHook(every uint64, fn func() error) error {
	defer vm.enter()()

	if fn == nil {
		return vm.wrap(vm.in.SetHook(0, nil))
	}
	if every == 0 {
		return errors.New("mruby: SetHook wants an interval of at least one instruction")
	}
	if every > math.MaxInt64 {
		return fmt.Errorf("mruby: SetHook interval %d is past the interpreter's largest, %d", every, int64(math.MaxInt64))
	}

	hook := func(*mrubyvm.Instance) int32 {
		vm.hostDepth.Add(1)
		defer vm.hostDepth.Add(-1)

		if err := fn(); err != nil {
			vm.raise(err)
			return 1
		}
		return 0
	}
	return vm.wrap(vm.in.SetHook(int64(every), hook))
}
