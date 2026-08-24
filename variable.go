package mruby

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
)

// DefineConst defines name as a constant under Object, taking the same value as an argument does.
func (vm *VM) DefineConst(name string, value any) error {
	defer vm.enter()()
	return vm.store(value, func() error { return vm.in.ConstSet(0, name) })
}

// Constant reads the constant at a `::`-separated path from Object, one namespace at a time; a constant that is not there is a Ruby NameError.
func (vm *VM) Constant(path string) (*Value, error) {
	segments, err := constPath(path)
	if err != nil {
		return nil, err
	}
	defer vm.enter()()

	outer, err := vm.walkConstPath(segments)
	if err != nil {
		return nil, err
	}
	defer vm.in.ReleaseRef(outer)

	raw, err := vm.in.ConstGet(outer, segments[len(segments)-1], false)
	if err != nil {
		return nil, vm.wrap(err)
	}
	return vm.capture(raw)
}

// ConstDefined answers whether the constant at a `::`-separated path from Object is there, walking the path the way Constant does.
// A segment that is not there is false; a segment that is there but is neither a Class nor a Module is the Ruby TypeError of looking a constant up in it.
func (vm *VM) ConstDefined(path string) (bool, error) {
	segments, err := constPath(path)
	if err != nil {
		return false, err
	}
	defer vm.enter()()

	var outer int32
	defer func() { vm.in.ReleaseRef(outer) }()
	for _, segment := range segments[:len(segments)-1] {
		ref, err := vm.namespaceRef(outer, segment)
		if err != nil {
			// The walk stopped here, so this segment is the answer: not there is false, and anything else is what the interpreter raised.
			return vm.constDefined(outer, segment)
		}
		vm.in.ReleaseRef(outer)
		outer = ref
	}
	return vm.constDefined(outer, segments[len(segments)-1])
}

// walkConstPath resolves every segment but the last, and answers the reference to the namespace that last one is read from, 0 for Object.
// The caller owns that reference.
func (vm *VM) walkConstPath(segments []string) (int32, error) {
	var outer int32
	for _, segment := range segments[:len(segments)-1] {
		ref, err := vm.namespaceRef(outer, segment)
		if err != nil {
			vm.in.ReleaseRef(outer)
			return 0, err
		}
		vm.in.ReleaseRef(outer)
		outer = ref
	}
	return outer, nil
}

func (vm *VM) namespaceRef(outer int32, segment string) (int32, error) {
	raw, err := vm.in.ConstGet(outer, segment, true)
	if err != nil {
		return 0, vm.wrap(err)
	}
	if raw.Ref == 0 {
		return 0, fmt.Errorf("mruby: the interpreter would not hold on to the constant %s", segment)
	}
	return raw.Ref, nil
}

// constDefined answers whether segment is there in the namespace behind outer, 0 for Object.
// The read comes first because mrb_const_get is what checks that the namespace is a class or module and mrb_const_defined is not (src/variable.c), so the question is only asked where that check has passed or where the name itself is refused before it.
func (vm *VM) constDefined(outer int32, segment string) (bool, error) {
	if _, err := vm.in.ConstGet(outer, segment, false); err != nil {
		var re *RubyError
		if wrapped := vm.wrap(err); !errors.As(wrapped, &re) || re.Class != "NameError" {
			return false, wrapped
		}
		defined, err := vm.in.ConstDefined(outer, segment)
		if err != nil {
			return false, vm.wrap(err)
		}
		return defined, nil
	}
	return true, nil
}

func constPath(path string) ([]string, error) {
	if path == "" {
		return nil, errors.New("mruby: the constant path is empty")
	}
	segments := strings.Split(path, "::")
	for _, segment := range segments {
		if segment == "" {
			return nil, fmt.Errorf("mruby: the constant path %q has an empty segment", path)
		}
	}
	return segments, nil
}

// GlobalVar reads the global variable name; the name crosses verbatim, `$` included, and one that was never set reads as Ruby nil.
func (vm *VM) GlobalVar(name string) (*Value, error) {
	defer vm.enter()()

	raw, err := vm.in.GlobalGet(name, false)
	if err != nil {
		return nil, vm.wrap(err)
	}
	return vm.capture(raw)
}

// SetGlobalVar stores value in the global variable name; the name crosses verbatim, `$` included.
func (vm *VM) SetGlobalVar(name string, value any) error {
	defer vm.enter()()
	return vm.store(value, func() error { return vm.in.GlobalSet(name) })
}

// InstanceVar reads the instance variable name from the value; the name crosses verbatim, `@` included, and one that was never set reads as Ruby nil.
func (v *Value) InstanceVar(name string) (*Value, error) {
	if v == nil {
		return nil, errors.New("mruby: InstanceVar on a value that is not there")
	}
	defer v.vm.enter()()

	b := builder{vm: v.vm}
	defer b.done()

	ref, err := b.receiver(v)
	if err != nil {
		return nil, err
	}
	defer runtime.KeepAlive(v)
	raw, err := v.vm.in.IVGet(ref, name, false)
	if err != nil {
		return nil, v.vm.wrap(err)
	}
	return v.vm.capture(raw)
}

// SetInstanceVar stores value in the instance variable name of the value; the name crosses verbatim, `@` included, and a value that holds no instance variables raises as Ruby does.
func (v *Value) SetInstanceVar(name string, value any) error {
	if v == nil {
		return errors.New("mruby: SetInstanceVar on a value that is not there")
	}
	defer v.vm.enter()()

	b := builder{vm: v.vm}
	defer b.done()

	ref, err := b.receiver(v)
	if err != nil {
		return err
	}
	if err := b.add(value); err != nil {
		return err
	}
	if err := b.flush(); err != nil {
		return err
	}
	defer runtime.KeepAlive(v)
	if err := v.vm.in.IVSet(ref, name); err != nil {
		return v.vm.wrap(err)
	}
	return nil
}

// store leaves value alone in the argument scratch, which is where the interpreter's setters take theirs, and runs the setter on it.
func (vm *VM) store(value any, set func() error) error {
	defer runtime.KeepAlive(value)
	b := builder{vm: vm}
	defer b.done()

	if err := b.add(value); err != nil {
		return err
	}
	if err := b.flush(); err != nil {
		return err
	}
	if err := set(); err != nil {
		return vm.wrap(err)
	}
	return nil
}
