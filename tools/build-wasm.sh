#!/usr/bin/env bash
#
# Build tools/cache/mruby_shim.wasm: mruby 4.0.0 plus tools/shim/shim.c, linked as a WASI reactor.
#
# mruby's raise/rescue/ensure lower to C setjmp/longjmp (include/mruby/throw.h) and wasm32 has no native setjmp/longjmp, so every object is compiled with LLVM's SJLJ lowering, which rewrites them into try_table/throw of the exception-handling proposal.
#
# zig 0.16 trap: passing -mexception-handling at LINK time makes zig build wasi-libc's own setjmp runtime (rt.c) on demand WITHOUT the -mllvm flags, and it dies ("undefined tag symbol cannot be weak").
# Workaround: compile rt.c here with the full flag set and feed rt.o straight into the link, which then runs with no EH flags at all.
# rt.c ships inside zig's own libc sysroot, so its path is derived from `zig env`, not hardcoded.
#
# The gem set is the one confirmed to compile clean on wasi (mruby-io, mruby-dir and mruby-socket cannot: they need <sys/wait.h>, <signal.h> and <sys/socket.h>, which wasi-libc does not provide), minus every mruby-bin-* command and plus mruby-compiler, which dm_eval needs.
# Losing mruby-io also loses Kernel#puts (it is mruby-io/mrblib/kernel.rb, not core); tools/mruby-wasi-puts restores it on top of core Kernel#print.
# mruby-bigint is what makes an integer literal past 2**31-1 work at all: mruby 4.0.0's parser reads a literal as int32 and hands everything wider to the bigint pool entry (mrbgems/mruby-compiler/core/parse.y, new_int), which without the gem is a RangeError when the VM loads it.
# Under a profile whose mrb_int is 32-bit it also carries the upper half of the ABI's i64 Integer range, so it is required there rather than merely useful.
#
# Re-running is a no-op while tools/cache/mruby_shim.stamp matches the inputs; FORCE=1 rebuilds from a clean tree, which is what the byte-identical rebuild is verified with.

set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"

MRUBY_VERSION=4.0.0
MRUBY_URL="https://github.com/mruby/mruby/archive/refs/tags/${MRUBY_VERSION}.tar.gz"
MRUBY_SHA256=e2ea271dbed14e9f2b33df773ae447b747dbc242ce2675022c0a57efea85a7b4

MRUBY_GEMS=(
  mruby-compiler mruby-sprintf mruby-math mruby-string-ext mruby-array-ext
  mruby-enum-ext mruby-hash-ext mruby-numeric-ext mruby-symbol-ext
  mruby-object-ext mruby-error mruby-metaprog mruby-pack mruby-random
  mruby-time mruby-exit mruby-bigint
)

# The three switches below (PROFILE, OPT, WASM_OPT) were measured against each other on an Apple M1 Pro, `go test -bench . -count=6`, best of six:
#
#   PROFILE   OPT  wasm-opt |    New  EvalArith  CallGo   Fib20  LeibnizPi |  wasm bytes  mruby_gen.go bytes
#   word      -O2  off      |  469us     2.70us  1.70us  9.43ms     2.25ms |     970,803          11,533,194
#   word      -Os  off      |  483us     2.79us  1.79us  9.56ms     2.18ms |     824,364          11,225,540
#   word      -Os  on       |  497us     3.04us  1.82us  9.56ms     2.14ms |     747,034          10,823,327
#   noboxing  -O2  off      |  773us     3.27us  2.84us 14.76ms     2.19ms |   1,260,958          14,513,317
#
# The interpreter loop is where the difference comes from: dewasm renders every wasm load and store as a Go method call with a bounds check, and a 16-byte mrb_value (a union plus its type tag) is two of them per copy where a word is one.
# The translated mrb_vm_exec holds 35,212 memory accessor calls under PROFILE=noboxing and 13,895 under PROFILE=word.
#
# PROFILE picks the mrb_value layout, which is what decides the interpreter's speed on this target.
# The ABI is the same in all of them: Integer crosses as i64, Float as f64, and where mrb_int is narrower than 64-bit the shim bridges the rest of the range through mruby-bigint (tools/shim/shim.c, int64_value and value_as_int64).
# mruby 4.0.0 refuses MRB_INT64 with word or NaN boxing on a 32-bit target (include/mrbconf.h, "MRB_INT64 on 32-bit requires MRB_NO_BOXING"), so a 64-bit mrb_int forces the 16-byte layout.
#
#   PROFILE=word       MRB_INT32 MRB_WORD_BOXING  4-byte mrb_value; Integer is a 31-bit fixnum, everything wider and every Float is a heap object.
#   PROFILE=noboxing32 MRB_INT32 MRB_NO_BOXING    8-byte mrb_value (a 4-byte union plus its tag); Integer is a 32-bit fixnum, Float is inline, wider Integer is a bigint.
#   PROFILE=noboxing   MRB_INT64 MRB_NO_BOXING    16-byte mrb_value; Integer is a 64-bit fixnum with no bigint in the middle of the range.
#
# MRB_NAN_BOXING is not among them: its 8-byte-aligned mrb_value grows struct RVALUE past the five words mruby 4.0.0's GC slot allows on a 32-bit target, and src/gc.c refuses to compile ("RVALUE size must be within 5 words", 24 <= 20).
#
# MRB_UTF8_STRING is in every profile: it makes String#size and friends count characters, matching Go's UTF-8 strings.
PROFILE=${PROFILE:-word}
case "$PROFILE" in
  word) BOXING_DEFINES=(MRB_INT32 MRB_WORD_BOXING) ;;
  noboxing32) BOXING_DEFINES=(MRB_INT32 MRB_NO_BOXING) ;;
  noboxing) BOXING_DEFINES=(MRB_INT64 MRB_NO_BOXING) ;;
  *)
    echo "build-wasm: PROFILE=$PROFILE is not one of word, noboxing32, noboxing" >&2
    exit 1
    ;;
esac
#
# MRB_USE_DEBUG_HOOK is what gives the shim an interruption point: it compiles mruby's code fetch hook into the interpreter loop, which is where tools/shim/shim.c raises a Ruby exception on behalf of a Go callback.
# It changes the mrb_state layout, and SHIM_DEFINES inherits MRUBY_DEFINES, so libmruby and the shim stay consistent.
# Its idle cost, with no hook armed, is one null check per VM instruction: 9.96ms on Fib20 against the 9.43ms of the table below, 5.6% slower.
MRUBY_DEFINES=("${BOXING_DEFINES[@]}" MRB_UTF8_STRING MRB_USE_DEBUG_HOOK)

# mruby-bigint defines MRB_USE_BIGINT for libmruby through its own mrbgem.rake; the shim is compiled outside that build and needs it to see the bigint entry points.
SHIM_DEFINES=("${MRUBY_DEFINES[@]}" MRB_USE_BIGINT)

EH_FLAGS=(-mexception-handling -mllvm -wasm-enable-sjlj -mllvm -wasm-use-legacy-eh=false)

# A C switch is as many nested wasm blocks as it has cases, dewasm renders each as a Go `for` block, and a `for` block costs two of the 1000 nested scopes go/parser allows, so 499 levels is the hard limit for the generated file.
# mruby 4.0.0's parser reduces 692 grammar rules in one switch and lands at 545 levels, which `go vet` (and `go test`, which vets) refuses to read at all: "exceeded max scope depth during object resolution".
# Compiling that one file without jump tables brings it to 336 levels; every other function in the module is far below the limit, and the interpreter loop's own dispatch keeps its jump table.
PARSER_SOURCE=mrbgems/mruby-compiler/core/y.tab.c
PARSER_FLAGS=(-fno-jump-tables)

# The C optimization level for everything in the module: libmruby (through the cc wrapper below, which is the only -O in the compile of each of its objects), the shim and rt.o.
# -O0 is not an option: the interpreter loop exceeds engine limits without optimization.
# zig cc has two release modes here, not four: -O1, -O2 and -O3 produce a byte-identical module, and -Os and -Oz produce the other one.
OPT=${OPT:--O2}
OPT_FLAGS=("$OPT")

# binaryen 132 parses the final (try_table/throw) exception-handling form the SJLJ lowering emits, which older binaryen could not, and its output passes the whole test suite.
# WASM_OPT=1 turns it on; it stays off because it trades speed for size, 9% off the module against 3% to 13% onto every benchmark.
WASM_OPT=${WASM_OPT:-0}
WASM_OPT_FLAGS=(
  -O2 --enable-exception-handling --enable-bulk-memory --enable-sign-ext
  --enable-nontrapping-float-to-int --enable-mutable-globals --enable-multivalue
  --enable-reference-types
)

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

# libkey is every input of libmruby.a, and key adds what only the shim and the link consume, so an input cannot sit in one list and be missing from the other.
libkey="mruby:$MRUBY_SHA256 gems:${MRUBY_GEMS[*]} defines:${MRUBY_DEFINES[*]} parser:${PARSER_FLAGS[*]} eh:${EH_FLAGS[*]} opt:$OPT"
libkey="$libkey zig:$(zig version) config:$(file_sha tools/mruby_build_config.rb) puts:$(tree_sha tools/mruby-wasi-puts)"
key="lib:[$libkey] stack:$STACK_SIZE wasm-opt:$WASM_OPT shim:$(file_sha tools/shim/shim.c)"

# rake tracks source timestamps and not compiler flags, so the flags choose the build directory instead: a changed libkey starts in an empty directory, and returning to an earlier libkey finds its objects again.
build_dir="$cache/mruby-build-$(printf '%s' "$libkey" | shasum -a 256 | cut -c1-12)"

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
  rm -rf "$src" "$cache"/mruby-build-*
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
  echo '# The parser is compiled with flags of its own; PARSER_FLAGS in tools/build-wasm.sh says why.'
  echo 'parser_flags='
  printf 'case "$*" in *%s*) parser_flags="%s" ;; esac\n' "$PARSER_SOURCE" "${PARSER_FLAGS[*]}"
  printf 'exec zig cc -target wasm32-wasi'
  printf ' %s' "${OPT_FLAGS[@]}" "${EH_FLAGS[@]}"
  printf ' $parser_flags "$@"\n'
} >"$cc_wrapper"

ar_wrapper="$cache/mruby-ar.sh"
printf '#!/bin/sh\nexec zig ar "$@"\n' >"$ar_wrapper"
chmod +x "$cc_wrapper" "$ar_wrapper"

echo "build-wasm: libmruby.a (rake, zig cc, LLVM SJLJ lowering)"
jobs=$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 1)
(
  cd "$src"
  MRUBY_CONFIG="$root/tools/mruby_build_config.rb" \
    MRUBY_BUILD_DIR="$build_dir" \
    DEWASM_MRUBY_CC="$cc_wrapper" \
    DEWASM_MRUBY_LD="$cc_wrapper" \
    DEWASM_MRUBY_AR="$ar_wrapper" \
    DEWASM_MRUBY_GEMS="${MRUBY_GEMS[*]}" \
    DEWASM_MRUBY_DEFINES="${MRUBY_DEFINES[*]}" \
    rake -j"$jobs"
)

echo "build-wasm: shim.o"
zig cc -target wasm32-wasi "${OPT_FLAGS[@]}" "${EH_FLAGS[@]}" \
  "${SHIM_DEFINES[@]/#/-D}" \
  -I "$src/include" -I "$build_dir/wasm32-wasi/include" \
  -c -o "$cache/shim.o" tools/shim/shim.c

# No EH flag reaches the link (the zig 0.16 trap above), and --strip-debug is what keeps the module from carrying full DWARF.
echo "build-wasm: linking $out"
zig cc -target wasm32-wasi -mexec-model=reactor \
  -Wl,--strip-debug -Wl,-z,stack-size="$STACK_SIZE" \
  -o "$out" "$cache/shim.o" "$cache/rt.o" "$build_dir/wasm32-wasi/lib/libmruby.a"

if [ "$WASM_OPT" != 0 ]; then
  require_tool wasm-opt "install binaryen 132 or newer (e.g. brew install binaryen)"
  echo "build-wasm: wasm-opt"
  wasm-opt "${WASM_OPT_FLAGS[@]}" "$out" -o "$out.opt"
  mv "$out.opt" "$out"
fi

printf '%s\n' "$key" >"$stamp"
echo "build-wasm: -> $out ($(wc -c <"$out" | tr -d ' ') bytes)"
