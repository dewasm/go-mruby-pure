package mruby_test

import (
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/dewasm/go-mruby"
)

func Example() {
	vm, err := mruby.New()
	if err != nil {
		log.Fatal(err)
	}
	v, err := vm.Eval(`"hello, %s" % "world"`)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(v)
	// Output: hello, world
}

func ExampleVM_Define() {
	vm, err := mruby.New()
	if err != nil {
		log.Fatal(err)
	}
	if err := vm.Define("shout", func(s string) string { return strings.ToUpper(s) + "!" }); err != nil {
		log.Fatal(err)
	}
	v, err := vm.Eval(`["hello", "go"].map { |w| shout(w) }.join(" ")`)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(v)
	// Output: HELLO! GO!
}

func ExampleVM_DefineClass() {
	vm, err := mruby.New()
	if err != nil {
		log.Fatal(err)
	}
	greeter, err := vm.DefineClass("Greeter", nil)
	if err != nil {
		log.Fatal(err)
	}
	err = greeter.DefineMethod("greet", func(c *mruby.Call) (any, error) {
		name, err := c.Arg(0).Text()
		if err != nil {
			return nil, mruby.Raise("TypeError", "greet wants a name")
		}
		return "hello, " + name, nil
	})
	if err != nil {
		log.Fatal(err)
	}

	fromRuby, err := vm.Eval(`Greeter.new.greet("world")`)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(fromRuby)

	instance, err := greeter.New()
	if err != nil {
		log.Fatal(err)
	}
	fromGo, err := instance.Call("greet", "Go")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(fromGo)
	// Output:
	// hello, world
	// hello, Go
}

func ExampleValue_Export() {
	vm, err := mruby.New()
	if err != nil {
		log.Fatal(err)
	}
	v, err := vm.Eval(`{"name" => "mruby", "versions" => [3, 4], :ok => true}`)
	if err != nil {
		log.Fatal(err)
	}
	x, err := v.Export()
	if err != nil {
		log.Fatal(err)
	}
	m := x.(map[any]any)
	fmt.Println(m["name"], m["versions"], m[mruby.Symbol("ok")])
	// Output: mruby [3 4] true
}

func ExampleRubyError() {
	vm, err := mruby.New()
	if err != nil {
		log.Fatal(err)
	}
	_, err = vm.Eval("def parse(s); raise ArgumentError, 'not a number: ' + s; end; parse('twelve')")

	var rubyErr *mruby.RubyError
	if errors.As(err, &rubyErr) {
		fmt.Println("class:  ", rubyErr.Class)
		fmt.Println("message:", rubyErr.Message)
		fmt.Println("where:  ", rubyErr.Backtrace[0])
	}
	// Output:
	// class:   ArgumentError
	// message: not a number: twelve
	// where:   (eval):1:in parse
}
