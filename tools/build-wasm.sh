#!/usr/bin/env bash
#
# Build tools/cache/mruby_shim.wasm: mruby 3.4.0 plus tools/shim/shim.c, linked as a WASI reactor.
#
# mruby's raise/rescue/ensure lower to C setjmp/longjmp (include/mruby/throw.h) and wasm32 has no native setjmp/longjmp, so every object is compiled with LLVM's SJLJ lowering, which rewrites them into try_table/throw of the exception-handling proposal.
# wasm-opt is never run on the result: it cannot parse exception handling.
#
# zig 0.16 trap: passing -mexception-handling at LINK time makes zig build wasi-libc's own setjmp runtime (rt.c) on demand WITHOUT the -mllvm flags, and it dies ("undefined tag symbol cannot be weak").
# Workaround: compile rt.c here with the full flag set and feed rt.o straight into the link, which then runs with no EH flags at all.
# rt.c ships inside zig's own libc sysroot, so its path is derived from `zig env`, not hardcoded.
#
# The gem set is the one confirmed to compile clean on wasi (mruby-io, mruby-dir and mruby-socket cannot: they need <sys/wait.h>, <signal.h> and <sys/socket.h>, which wasi-libc does not provide), minus every mruby-bin-* command and plus mruby-compiler, which dm_eval needs.
# Losing mruby-io also loses Kernel#puts (it is mruby-io/mrblib/kernel.rb, not core); tools/mruby-wasi-puts restores it on top of core Kernel#print.
#
# Re-running is a no-op while tools/cache/mruby_shim.stamp matches the inputs; FORCE=1 rebuilds from a clean tree, which is what the byte-identical rebuild is verified with.

set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"

MRUBY_VERSION=3.4.0
MRUBY_URL="https://github.com/mruby/mruby/archive/refs/tags/${MRUBY_VERSION}.tar.gz"
MRUBY_SHA256=183711c7a26d932b5342e64860d16953f1cc6518d07b2c30a02937fb362563f8

MRUBY_GEMS=(
  mruby-compiler mruby-sprintf mruby-math mruby-string-ext mruby-array-ext
  mruby-enum-ext mruby-hash-ext mruby-numeric-ext mruby-symbol-ext
  mruby-object-ext mruby-error mruby-metaprog mruby-pack mruby-random
  mruby-time mruby-exit
)

# MRB_INT64 is required by the ABI (Integer crosses as i64) and MRB_UTF8_STRING makes String#size and friends count characters, matching Go's UTF-8 strings.
MRUBY_DEFINES=(MRB_INT64 MRB_UTF8_STRING)

EH_FLAGS=(-mexception-handling -mllvm -wasm-enable-sjlj -mllvm -wasm-use-legacy-eh=false)

# -O0 is not an option: the interpreter loop exceeds engine limits without optimization.
OPT_FLAGS=(-O2)

STACK_SIZE=1048576

cache="$root/tools/cache"
src="$cache/mruby-$MRUBY_VERSION"
out="$cache/mruby_shim.wasm"
stamp="$cache/mruby_shim.stamp"
tarball="$cache/mruby-$MRUBY_VERSION.tar.gz"

require_tool() {
  command -v "$1" >/dev/null && return
  echo "build-wasm: $1 not found: $2" >&2
  exit 1
}

require_tool zig "install zig 0.16 (e.g. brew install zig)"
require_tool ruby "install a host Ruby to run mruby's rake build"
require_tool rake "install rake (e.g. gem install rake)"
require_tool shasum "shasum is part of the base system"

file_sha() { shasum -a 256 "$1" | cut -d' ' -f1; }
tree_sha() { find "$1" -type f -print0 | sort -z | xargs -0 shasum -a 256 | shasum -a 256 | cut -d' ' -f1; }

key="mruby:$MRUBY_SHA256 gems:${MRUBY_GEMS[*]} defines:${MRUBY_DEFINES[*]} stack:$STACK_SIZE"
key="$key zig:$(zig version) shim:$(file_sha tools/shim/shim.c)"
key="$key config:$(file_sha tools/mruby_build_config.rb) puts:$(tree_sha tools/mruby-wasi-puts)"

mkdir -p "$cache"
if [ -z "${FORCE:-}" ] && [ -f "$out" ] && [ "$(cat "$stamp" 2>/dev/null || true)" = "$key" ]; then
  echo "build-wasm: up to date ($out)"
  exit 0
fi

if [ ! -f "$tarball" ] || [ "$(file_sha "$tarball")" != "$MRUBY_SHA256" ]; then
  echo "build-wasm: fetching $MRUBY_URL"
  curl -fsSL --retry 5 --retry-delay 2 --retry-connrefused -o "$tarball" "$MRUBY_URL"
  echo "$MRUBY_SHA256  $tarball" | shasum -a 256 -c - >/dev/null
fi

if [ -n "${FORCE:-}" ]; then
  rm -rf "$src"
fi
if [ ! -d "$src" ]; then
  echo "build-wasm: unpacking mruby $MRUBY_VERSION"
  tar xzf "$tarball" -C "$cache"
fi

zig_lib_dir=$(zig env | sed -n 's/.*\.lib_dir = "\(.*\)",/\1/p')
rt_c="$zig_lib_dir/libc/wasi/libc-top-half/musl/src/setjmp/wasm32/rt.c"
[ -f "$rt_c" ] || {
  echo "build-wasm: zig's wasi setjmp runtime not found at $rt_c (zig layout changed?)" >&2
  exit 1
}

echo "build-wasm: rt.o (zig's wasi setjmp runtime, with SJLJ lowering)"
zig cc -target wasm32-wasi "${OPT_FLAGS[@]}" "${EH_FLAGS[@]}" -c -o "$cache/rt.o" "$rt_c"

cc_wrapper="$cache/mruby-cc.sh"
{
  echo '#!/bin/sh'
  printf 'exec zig cc -target wasm32-wasi'
  printf ' %s' "${OPT_FLAGS[@]}" "${EH_FLAGS[@]}"
  printf ' "$@"\n'
} >"$cc_wrapper"

ar_wrapper="$cache/mruby-ar.sh"
printf '#!/bin/sh\nexec zig ar "$@"\n' >"$ar_wrapper"
chmod +x "$cc_wrapper" "$ar_wrapper"

echo "build-wasm: libmruby.a (rake, zig cc, LLVM SJLJ lowering)"
jobs=$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 1)
(
  cd "$src"
  MRUBY_CONFIG="$root/tools/mruby_build_config.rb" \
    DEWASM_MRUBY_CC="$cc_wrapper" \
    DEWASM_MRUBY_LD="$cc_wrapper" \
    DEWASM_MRUBY_AR="$ar_wrapper" \
    DEWASM_MRUBY_GEMS="${MRUBY_GEMS[*]}" \
    DEWASM_MRUBY_DEFINES="${MRUBY_DEFINES[*]}" \
    rake -j"$jobs"
)

echo "build-wasm: shim.o"
zig cc -target wasm32-wasi "${OPT_FLAGS[@]}" "${EH_FLAGS[@]}" \
  "${MRUBY_DEFINES[@]/#/-D}" \
  -I "$src/include" -I "$src/build/wasm32-wasi/include" \
  -c -o "$cache/shim.o" tools/shim/shim.c

# No EH flag reaches the link (the zig 0.16 trap above), and --strip-debug is what keeps the module from carrying full DWARF.
echo "build-wasm: linking $out"
zig cc -target wasm32-wasi -mexec-model=reactor \
  -Wl,--strip-debug -Wl,-z,stack-size="$STACK_SIZE" \
  -o "$out" "$cache/shim.o" "$cache/rt.o" "$src/build/wasm32-wasi/lib/libmruby.a"

printf '%s\n' "$key" >"$stamp"
echo "build-wasm: -> $out ($(wc -c <"$out" | tr -d ' ') bytes)"
