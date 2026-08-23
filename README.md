# go-mruby-pure

**mruby** for **Go** *without cgo*.

The [mruby](https://mruby.org) interpreter is compiled to WebAssembly (WASI) and translated to **pure Go** source by [`dewasm`](https://github.com/dewasm/dewasm), so this library needs *no C toolchain*, no *shared libraries*, and *no cgo*.
`go get` is the whole installation, cross compilation keeps working, and the interpreter runs inside your process as ordinary Go code.
It shares no code with [mitchellh/go-mruby](https://github.com/mitchellh/go-mruby), the archived cgo bindings, but covers its ground; the table under "Coming from mitchellh/go-mruby" maps the two.

```go
vm, err := mruby.New()
if err != nil { ... }

v, err := vm.Eval(`"hello, %s" % "world"`)
fmt.Println(v) // hello, world
```

## Features

- **Every Ruby value crosses as a value or a handle, never as a string.**
  `*Value` carries immediates (nil, booleans, integers, floats, strings, symbols) by value and everything else as a garbage-collected handle; `GoValue` expands arrays and hashes into `[]any` and `map[any]any` recursively, with cycle detection.
- **Go functions become Ruby methods.**
  Reflection over an ordinary Go signature (`func(string) (string, error)` and the like; an `error` return raises in Ruby), or an explicit form receiving self, arguments, and the block.
  Blocks are first class: a host function can yield, Ruby Procs are callable from Go, and `CallWithBlock` passes a Proc as a method's block.

  ```go
  vm.Define("shout", func(s string) string { return strings.ToUpper(s) + "!" })
  vm.Eval(`["hello", "go"].map { |w| shout(w) }.join(" ")`) // HELLO! GO!
  ```

- **Ruby exceptions are Go errors.**
  Every crossing path surfaces them as `*mruby.RubyError` (class, message, backtrace) through `errors.As`; `mruby.Raise` lets a host function raise a chosen class, and `WithFilename` names an `Eval`'s source so backtraces point at your file.
- **Classes, modules, constants.**
  `DefineClass` and `DefineModule` build namespaces whose methods are Go functions (`DefineMethod`, `DefineClassMethod`, `DefineModuleFunction`), nested through the same calls on a `Class` or `Module`; `DefineConst` sets constants and `Class.New` instantiates.
- **The interpreter's state is reachable without evaluating source.**
  `Constant` and `ConstDefined` resolve `A::B` paths; `GlobalVar`/`SetGlobalVar` and `Value.InstanceVar`/`SetInstanceVar` read and write variables.
- **`Decode` fills Go structs.**
  A Ruby Hash, or any object with reader methods, decodes into a struct field by field, guided by the `mruby` tag, with `,squash` for embedded structs.
- **Isolated, capturable, concurrent.**
  Each `VM` is an independent interpreter; `WithStdout`/`WithStderr` route its output to any `io.Writer`; VM methods are safe for concurrent use (calls are serialized internally).
- **A runaway script is interruptible.**
  `SetHook` runs a Go callback every N VM instructions; an error it returns raises at an instruction boundary, so an endless `loop { }` comes back as a Ruby exception, `ensure` blocks run, and the VM survives.

## Coming from mitchellh/go-mruby

| mitchellh/go-mruby | here |
| --- | --- |
| `NewMrb()`, `Close()` | `New()`; there is nothing to close, the VM is garbage collected |
| `LoadString`, `Parser` + `CompileContext` | `Eval(src)`, `Eval(src, WithFilename("app.rb"))` |
| `Func` + `GetArgs` + `ArgSpec` | any Go function via reflection, or `func(*mruby.Call) (any, error)` |
| `DefineClass`, `DefineModule`, `DefineClassUnder`, `DefineModuleUnder` | `DefineClass`/`DefineModule` on `VM`, `Class`, and `Module` |
| `DefineClassMethod`, `DefineConst` | the same names, on `Class` and `Module` |
| `GetGlobalVariable`, `SetGlobalVariable` | `GlobalVar`, `SetGlobalVar` |
| `GetInstanceVariable`, `SetInstanceVariable` | `Value.InstanceVar`, `Value.SetInstanceVar` |
| `MrbValue.Call`, `CallBlock` | `Value.Call`, `Value.CallWithBlock` |
| `Decode` | `Decode`, the same `mruby` tag with `,squash` |
| `Exception` | `*RubyError`, matched with `errors.As` |
| `Yield` | `Call.Block().Call("call", args...)` |
| `ArenaSave`, `ArenaRestore`, `GCProtect`, `IsDead` | not needed: handles are garbage collected, `Value.Release` and `VM.FullGC` free eagerly |
| `Fixnum()`, `Float()`, `String()` (unchecked) | `AsInt`, `AsFloat`, `AsString`, `AsSymbol`, `AsBool` (type checked), `GoValue` |

## How it works

mruby 4.0.0 plus a small C shim is compiled with `zig cc` for `wasm32-wasi` (setjmp/longjmp lowers onto the WebAssembly exception-handling proposal) and translated by the dewasm Go backend into the committed `mrubyvm/` package.
The public API is a thin layer over that generated interpreter; there is no runtime dependency on wasm tooling, dewasm, or zig.

Regenerating the interpreter (only needed when changing the shim, the gem set, or the dewasm revision):

```console
$ tools/build-wasm.sh   # needs zig, ruby, rake; reproducible byte for byte
$ tools/convert.sh      # needs a dewasm checkout; pins its revision
```

## Performance

Measured on Apple M1 Pro (`go test -bench .`):

| Benchmark | Time |
| --- | --- |
| `New` (fresh interpreter) | 0.48 ms |
| `Eval` of an arithmetic expression | 2.7 µs |
| Ruby calling a Go-defined method | 1.7 µs |
| `fib(20)` in Ruby | 9.8 ms |
| `fib(20)` with the hook armed (`SetHook`, any interval) | 24.5 ms |

Integers within ±2³⁰ and all value moves take the interpreter's fast path; a wider Integer is an arbitrary-precision object inside Ruby, and carrying one across the Go boundary costs about a microsecond extra.

## Limits

- The embedded mruby excludes the gems that need an operating system (`mruby-io`, `mruby-dir`, `mruby-socket`); the interpreter computes, and your Go code does the I/O through host functions.
- While a host function is running, only the goroutine inside it may use that VM.
- `SetHook` interrupts Ruby-level execution only: a long-running C routine inside the interpreter (a huge arbitrary-precision multiplication, for example) and a blocking Go host function run to completion.
- While the hook is armed the interpreter runs at less than half speed, whatever the interval, so arm it only around the execution that needs a budget.

## License

The MIT license ([LICENSE](LICENSE)).

The generated `mrubyvm/` package embeds mruby, which is MIT licensed ([LICENSE-MRUBY](LICENSE-MRUBY)).
