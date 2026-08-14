# go-mruby

mruby for Go without cgo.

The [mruby](https://mruby.org) interpreter is compiled to WebAssembly (WASI) and translated to pure Go source by [dewasm](https://github.com/dewasm/dewasm), so this library needs no C toolchain, no shared libraries, and no cgo.
`go get` is the whole installation, cross compilation keeps working, and the interpreter runs inside your process as ordinary Go code.

```go
vm, err := mruby.New()
if err != nil { ... }

v, err := vm.Eval(`"hello, %s" % "world"`)
fmt.Println(v) // hello, world
```

## Features

- **Every Ruby value crosses as a value or a handle, never as a string.**
  `*Value` carries immediates (nil, booleans, integers, floats, strings, symbols) by value and everything else as a garbage-collected handle; `Export` expands arrays and hashes into `[]any` and `map[any]any` recursively, with cycle detection.
- **Go functions become Ruby methods.**
  Reflection over an ordinary Go signature (`func(string) (string, error)` and the like; an `error` return raises in Ruby), or an explicit form receiving self, arguments, and the block.
  Blocks are first class: a host function can yield, and Ruby Procs are callable from Go.

  ```go
  vm.Define("shout", func(s string) string { return strings.ToUpper(s) + "!" })
  vm.Eval(`["hello", "go"].map { |w| shout(w) }.join(" ")`) // HELLO! GO!
  ```

- **Ruby exceptions are Go errors.**
  Every crossing path surfaces them as `*mruby.RubyError` (class, message, backtrace) through `errors.As`; `mruby.Raise` lets a host function raise a chosen class.
- **Classes.**
  `DefineClass` and `DefineMethod` build Ruby classes whose methods are Go functions; `Class.New` instantiates them.
- **Isolated, capturable, concurrent.**
  Each `VM` is an independent interpreter; `WithStdout`/`WithStderr` route its output to any `io.Writer`; VM methods are safe for concurrent use (calls are serialized internally).

## How it works

mruby 3.4.0 plus a small C shim is compiled with `zig cc` for `wasm32-wasi` (setjmp/longjmp lowers onto the WebAssembly exception-handling proposal) and translated by the dewasm Go backend into the committed `mrubyvm/` package.
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
| `New` (fresh interpreter) | 0.62 ms |
| `Eval` of an arithmetic expression | 3.2 µs |
| Ruby calling a Go-defined method | 1.6 µs |
| `fib(20)` in Ruby | 9.7 ms |

## Limits

- The embedded mruby excludes the gems that need an operating system (`mruby-io`, `mruby-dir`, `mruby-socket`); the interpreter computes, and your Go code does the I/O through host functions.
- While a host function is running, only the goroutine inside it may use that VM.
- `Eval` is not interruptible.

## License

MIT ([LICENSE](LICENSE)).
The generated `mrubyvm/` package embeds mruby, which is MIT licensed ([LICENSE.mruby](LICENSE.mruby)).
