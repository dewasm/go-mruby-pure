#!/bin/sh

set -eu

flags="${OPT:--O2} -mexception-handling -mllvm -wasm-enable-sjlj -mllvm -wasm-use-legacy-eh=false"
case "$*" in
*mrbgems/mruby-compiler/core/y.tab.c*) flags="$flags -fno-jump-tables" ;;
esac

exec zig cc -target wasm32-wasi $flags "$@"
