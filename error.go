package mruby

import (
	"fmt"

	"github.com/dewasm/go-mruby/mrubyvm"
)

// RubyError is a Ruby exception that reached Go.
type RubyError struct {
	Class     string
	Message   string
	Backtrace []string
}

func (e *RubyError) Error() string {
	return e.Class + ": " + e.Message
}

// Raise is the error a host function returns to raise class in Ruby.
// Any other non-nil error raises RuntimeError with the error's text, so returning a *RubyError from a nested call re-raises it unchanged.
func Raise(class, format string, a ...any) error {
	return &RubyError{Class: class, Message: fmt.Sprintf(format, a...)}
}

// ExitError is Ruby ending the interpreter with Kernel#exit!, after which the VM answers nothing else.
type ExitError = mrubyvm.ExitError

// TrapError is the translated interpreter trapping: a bug or a resource limit, never a Ruby-level condition.
type TrapError = mrubyvm.TrapError

// ABIError is a misuse of the interpreter boundary, such as a released value used again.
type ABIError = mrubyvm.ABIError
