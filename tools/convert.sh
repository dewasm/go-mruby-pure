#!/usr/bin/env bash
#
# Translate tools/cache/mruby_shim.wasm into mrubyvm/mruby_gen.go plus its data sidecar mrubyvm/mruby_gen.dat, both committed generated source: never edit them by hand, re-run this.
#
# The generated file declares `package mrubyvm` and the type `Mrubyvm`; mrubyvm/host.go is hand-written and declares the same package, which is how it reaches the instance's linear memory and the runtime's trap/exit types (all unexported).
#
# Pinned dewasm revision: 2e4d94571fe53027aa49452e6da68feaa9eaf99e
# A different revision is a warning, not an error: regenerating against a newer dewasm is how this library picks up backend fixes, and the Go tests are what decide whether the result is good.

set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"

DEWASM_REV=2e4d94571fe53027aa49452e6da68feaa9eaf99e
DEWASM_REPO=${DEWASM_REPO:-$root/../dewasm}

wasm="tools/cache/mruby_shim.wasm"
[ -f "$wasm" ] || {
  echo "convert: $wasm is missing; run tools/build-wasm.sh first" >&2
  exit 1
}
[ -f "$DEWASM_REPO/Cargo.toml" ] || {
  echo "convert: no dewasm checkout at $DEWASM_REPO (set DEWASM_REPO)" >&2
  exit 1
}

have=$(git -C "$DEWASM_REPO" rev-parse HEAD 2>/dev/null || echo unknown)
if [ "$have" != "$DEWASM_REV" ]; then
  echo "convert: warning: dewasm is at $have, this output was pinned to $DEWASM_REV" >&2
fi

mkdir -p mrubyvm
cargo run -q --release --manifest-path "$DEWASM_REPO/Cargo.toml" -p dewasm-cli -- \
  "$wasm" --target go --mode library --module-name Mrubyvm \
  --data-file mrubyvm/mruby_gen.dat -o mrubyvm/mruby_gen.go

# dewasm's bundled runtime units are written with spaces, so the generated file is not gofmt-canonical as emitted; formatting it here keeps `gofmt -l .` empty for the whole repository.
gofmt -w mrubyvm/mruby_gen.go

# dewasm's Go backend emits a statement after a terminating one where the wasm block structure calls for it (`break L3` after `break L1`, `return 0` after a `return`), which `go vet` reports as unreachable code, thousands of times.
# Unreachable statements cannot run, so dropping them cannot change behavior; this drops exactly the ones vet points at, refuses any shape it was not built for, and rebuilds after each round, so a wrong drop fails here instead of shipping.
# Rounds are needed because vet reports only the first statement of each unreachable run.
strip_unreachable() {
  local vet=$1 lines=$2 round
  for round in 1 2 3 4 5; do
    if go vet ./mrubyvm >"$vet" 2>&1; then
      return 0
    fi
    grep -oE 'mruby_gen\.go:[0-9]+:[0-9]+: ' "$vet" | cut -d: -f2 | sort -nu >"$lines"
    if [ ! -s "$lines" ]; then
      echo "convert: go vet failed for something other than the generated file:" >&2
      cat "$vet" >&2
      return 1
    fi
    awk -v list="$lines" '
      BEGIN { while ((getline n < list) > 0) drop[n] = 1 }
      NR in drop {
        s = $0
        sub(/^[ \t]+/, "", s)
        if (s !~ /^(break L[0-9]+|goto L[0-9]+|continue L[0-9]+|return .*|panic\(__r\)|l[0-9]+ = l[0-9]+)$/) {
          printf "convert: unexpected unreachable statement at line %d: %s\n", NR, s > "/dev/stderr"
          bad = 1
        }
      }
      END { exit bad }
    ' mrubyvm/mruby_gen.go || return 1
    awk -v list="$lines" '
      BEGIN { while ((getline n < list) > 0) drop[n] = 1 }
      !(NR in drop)
    ' mrubyvm/mruby_gen.go >mrubyvm/mruby_gen.go.stripped
    mv mrubyvm/mruby_gen.go.stripped mrubyvm/mruby_gen.go
    echo "convert: dropped $(wc -l <"$lines" | tr -d ' ') unreachable statements"
    strip_unused_declarations "$vet" "$lines" || return 1
    go build ./... || {
      echo "convert: dropping unreachable statements broke the build" >&2
      return 1
    }
  done
  echo "convert: go vet still reports the generated file after 5 rounds" >&2
  return 1
}

# A dropped statement can be the only reader of a local declaration, which Go then refuses to compile.
# The compiler names each one, and a declaration it calls unused cannot be doing anything, so this drops exactly those, refuses any shape it was not built for, and repeats because one round can orphan the next.
strip_unused_declarations() {
  local build=$1 lines=$2 round
  for round in 1 2 3 4 5; do
    if go build ./mrubyvm >"$build" 2>&1; then
      return 0
    fi
    grep -oE 'mruby_gen\.go:[0-9]+:[0-9]+: declared and not used' "$build" | cut -d: -f2 | sort -nu >"$lines"
    if [ ! -s "$lines" ]; then
      echo "convert: go build failed for something other than an unused declaration:" >&2
      cat "$build" >&2
      return 1
    fi
    awk -v list="$lines" '
      BEGIN { while ((getline n < list) > 0) drop[n] = 1 }
      NR in drop {
        s = $0
        sub(/^[ \t]+/, "", s)
        if (s !~ /^var [A-Za-z_][A-Za-z_0-9]* [a-z0-9]+$/) {
          printf "convert: unexpected unused declaration at line %d: %s\n", NR, s > "/dev/stderr"
          bad = 1
        }
      }
      END { exit bad }
    ' mrubyvm/mruby_gen.go || return 1
    awk -v list="$lines" '
      BEGIN { while ((getline n < list) > 0) drop[n] = 1 }
      !(NR in drop)
    ' mrubyvm/mruby_gen.go >mrubyvm/mruby_gen.go.stripped
    mv mrubyvm/mruby_gen.go.stripped mrubyvm/mruby_gen.go
    echo "convert: dropped $(wc -l <"$lines" | tr -d ' ') unused declarations"
  done
  echo "convert: go build still reports unused declarations after 5 rounds" >&2
  return 1
}

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
strip_unreachable "$tmp/vet" "$tmp/lines"

echo "convert: -> mrubyvm/mruby_gen.go ($(wc -c <mrubyvm/mruby_gen.go | tr -d ' ') bytes), mrubyvm/mruby_gen.dat ($(wc -c <mrubyvm/mruby_gen.dat | tr -d ' ') bytes)"
