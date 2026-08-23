package mruby_test

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/dewasm/go-mruby-pure"
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
		name, err := c.Arg(0).AsString()
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

func ExampleVM_DefineModule() {
	vm, err := mruby.New()
	if err != nil {
		log.Fatal(err)
	}
	text, err := vm.DefineModule("Text")
	if err != nil {
		log.Fatal(err)
	}
	if err := text.DefineModuleFunction("shout", strings.ToUpper); err != nil {
		log.Fatal(err)
	}
	if err := text.DefineConst("MARK", "!"); err != nil {
		log.Fatal(err)
	}

	banner, err := text.DefineClass("Banner", nil)
	if err != nil {
		log.Fatal(err)
	}
	if err := banner.DefineClassMethod("of", func(s string) string { return "[" + s + "]" }); err != nil {
		log.Fatal(err)
	}

	fromRuby, err := vm.Eval(`Text::Banner.of(Text.shout("hello") + Text::MARK)`)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(fromRuby)

	mark, err := vm.Constant("Text::MARK")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(mark)
	// Output:
	// [HELLO!]
	// !
}

func ExampleVM_SetHook() {
	vm, err := mruby.New()
	if err != nil {
		log.Fatal(err)
	}
	// An Exception, not a StandardError: a bare rescue in the script must not swallow the deadline.
	if _, err := vm.Eval("class Deadline < Exception; end"); err != nil {
		log.Fatal(err)
	}

	deadline := time.Now().Add(50 * time.Millisecond)
	err = vm.SetHook(1000, func() error {
		if time.Now().Before(deadline) {
			return nil
		}
		return mruby.Raise("Deadline", "the script ran out of time")
	})
	if err != nil {
		log.Fatal(err)
	}

	_, err = vm.Eval("loop { }")
	fmt.Println(err)
	// Output: Deadline: the script ran out of time
}

func ExampleValue_CallWithBlock() {
	vm, err := mruby.New()
	if err != nil {
		log.Fatal(err)
	}
	list, err := vm.Eval("[1, 2, 3]")
	if err != nil {
		log.Fatal(err)
	}
	block, err := vm.Eval("->(n) { n * n }")
	if err != nil {
		log.Fatal(err)
	}
	squares, err := list.CallWithBlock("map", block)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(squares.Inspect())
	// Output: [1, 4, 9]
}

func ExampleValue_GoValue() {
	vm, err := mruby.New()
	if err != nil {
		log.Fatal(err)
	}
	v, err := vm.Eval(`{"name" => "mruby", "versions" => [4, 0], :ok => true}`)
	if err != nil {
		log.Fatal(err)
	}
	x, err := v.GoValue()
	if err != nil {
		log.Fatal(err)
	}
	m := x.(map[any]any)
	fmt.Println(m["name"], m["versions"], m[mruby.Symbol("ok")])
	// Output: mruby [4 0] true
}

func ExampleVM_ToValue() {
	vm, err := mruby.New()
	if err != nil {
		log.Fatal(err)
	}
	hash, err := vm.ToValue(map[string]any{"one": 1})
	if err != nil {
		log.Fatal(err)
	}
	size, err := hash.Call("size")
	if err != nil {
		log.Fatal(err)
	}
	n, err := size.AsInt()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(hash.Inspect(), n)
	// Output: {"one" => 1} 1
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
