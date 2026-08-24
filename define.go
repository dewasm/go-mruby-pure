package mruby

import (
	"errors"
	"fmt"
	"runtime"

	"github.com/dewasm/go-mruby-pure/mrubyvm"
)

// DefineClass defines name as a class under Object, deriving from super, or from Object when super is nil.
func (vm *VM) DefineClass(name string, super *Class) (*Class, error) {
	defer vm.enter()()

	superRef, err := vm.superRef(super)
	if err != nil {
		return nil, err
	}
	defer runtime.KeepAlive(super)
	ref, err := vm.in.DefineClass(name, superRef)
	if err != nil {
		return nil, vm.wrap(err)
	}
	return vm.newClass(name, ref)
}

// DefineModule defines name as a module under Object.
func (vm *VM) DefineModule(name string) (*Module, error) {
	defer vm.enter()()

	ref, err := vm.in.DefineModule(name)
	if err != nil {
		return nil, vm.wrap(err)
	}
	return vm.newModule(name, ref)
}

// Class is a Ruby class defined from Go.
type Class struct {
	namespace
}

// New makes an instance, passing the arguments on to initialize.
func (c *Class) New(args ...any) (*Value, error) {
	defer c.vm.enter()()
	return c.vm.call(c.value, "new", args)
}

// DefineClassMethod defines name as a method of the class itself, which Ruby reaches as C.name, taking the same fn as VM.Define.
func (c *Class) DefineClassMethod(name string, fn any) error {
	defer c.vm.enter()()

	id, err := c.vm.register(c.name+"."+name, fn)
	if err != nil {
		return err
	}
	defer runtime.KeepAlive(c.value)
	if err := c.vm.in.DefineSingletonMethod(c.value.ref, name, id); err != nil {
		return c.vm.wrap(err)
	}
	return nil
}

// Module is a Ruby module defined from Go.
type Module struct {
	namespace
}

// DefineModuleFunction defines name both as a method of the module itself, which Ruby reaches as M.name, and as an instance method a class that includes the module inherits.
// It takes the same fn as VM.Define.
func (m *Module) DefineModuleFunction(name string, fn any) error {
	defer m.vm.enter()()

	id, err := m.vm.register(m.name+"."+name, fn)
	if err != nil {
		return err
	}
	defer runtime.KeepAlive(m.value)
	if err := m.vm.in.DefineModuleFunction(m.value.ref, name, id); err != nil {
		return m.vm.wrap(err)
	}
	return nil
}

// namespace is what a Class and a Module have in common: a Ruby class or module held from Go, and the definitions made on it.
type namespace struct {
	vm    *VM
	name  string
	value *Value
}

// Value is the class or module object itself, for sending it a message or passing it on.
func (n *namespace) Value() *Value {
	return n.value
}

// DefineMethod defines name as an instance method of the class or module, taking the same fn as VM.Define.
func (n *namespace) DefineMethod(name string, fn any) error {
	defer n.vm.enter()()

	id, err := n.vm.register(n.name+"#"+name, fn)
	if err != nil {
		return err
	}
	defer runtime.KeepAlive(n.value)
	if err := n.vm.in.DefineMethod(n.value.ref, name, id); err != nil {
		return n.vm.wrap(err)
	}
	return nil
}

// DefineConst defines name as a constant of the class or module, taking the same value as an argument does.
func (n *namespace) DefineConst(name string, value any) error {
	defer n.vm.enter()()
	return n.vm.store(value, func() error { return n.vm.in.ConstSet(n.value.ref, name) })
}

// DefineClass defines name as a class nested in the class or module, deriving from super, or from Object when super is nil.
func (n *namespace) DefineClass(name string, super *Class) (*Class, error) {
	defer n.vm.enter()()

	superRef, err := n.vm.superRef(super)
	if err != nil {
		return nil, err
	}
	defer runtime.KeepAlive(n.value)
	defer runtime.KeepAlive(super)
	ref, err := n.vm.in.DefineClassUnder(n.value.ref, name, superRef)
	if err != nil {
		return nil, n.vm.wrap(err)
	}
	return n.vm.newClass(n.name+"::"+name, ref)
}

// DefineModule defines name as a module nested in the class or module.
func (n *namespace) DefineModule(name string) (*Module, error) {
	defer n.vm.enter()()

	defer runtime.KeepAlive(n.value)
	ref, err := n.vm.in.DefineModuleUnder(n.value.ref, name)
	if err != nil {
		return nil, n.vm.wrap(err)
	}
	return n.vm.newModule(n.name+"::"+name, ref)
}

func (vm *VM) superRef(super *Class) (int32, error) {
	if super == nil {
		return 0, nil
	}
	if super.vm != vm {
		return 0, errors.New("mruby: the superclass belongs to another VM")
	}
	return super.value.ref, nil
}

func (vm *VM) newClass(name string, ref int32) (*Class, error) {
	if ref == 0 {
		return nil, fmt.Errorf("mruby: the interpreter would not hold on to class %s", name)
	}
	return &Class{namespace{vm: vm, name: name, value: vm.handle(mrubyvm.KindOther, ref)}}, nil
}

func (vm *VM) newModule(name string, ref int32) (*Module, error) {
	if ref == 0 {
		return nil, fmt.Errorf("mruby: the interpreter would not hold on to module %s", name)
	}
	return &Module{namespace{vm: vm, name: name, value: vm.handle(mrubyvm.KindOther, ref)}}, nil
}
