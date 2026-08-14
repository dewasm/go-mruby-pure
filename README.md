# go-mruby

mruby for Go without cgo.

The [mruby](https://mruby.org) interpreter is compiled to WebAssembly (WASI) and translated to pure Go source by [dewasm](https://github.com/dewasm/dewasm), so this library needs no C toolchain, no shared libraries, and no cgo: `go get` is the whole installation.

Work in progress; the v0.1 API is under construction.
