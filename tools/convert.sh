#!/usr/bin/env bash
#
# Translate tools/cache/mruby_shim.wasm into mrubyvm/mruby_gen.go plus its data sidecar mrubyvm/mruby_gen.dat, both committed generated source: never edit them by hand, re-run this.
#
# The generated file declares `package mrubyvm` and the type `Mrubyvm`; mrubyvm/host.go is hand-written and declares the same package, which is how it reaches the instance's linear memory and the runtime's trap/exit types (all unexported).
#
# Pinned dewasm revision: 0e012ef0acab735469a04a36a12ad576f7f14540
# A different revision is a warning, not an error: regenerating against a newer dewasm is how this library picks up backend fixes, and the Go tests are what decide whether the result is good.

set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"

DEWASM_REV=0e012ef0acab735469a04a36a12ad576f7f14540
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


# dewasm stopped emitting unreachable statements and self-assignments (dewasm#214),
# so the generated file must pass go vet as produced; a diagnostic here means a
# dewasm regression, not something to strip around.
if ! go vet ./mrubyvm; then
  echo "convert: go vet rejects the generated file; the pinned dewasm should produce vet-clean output" >&2
  exit 1
fi

echo "convert: -> mrubyvm/mruby_gen.go ($(wc -c <mrubyvm/mruby_gen.go | tr -d ' ') bytes), mrubyvm/mruby_gen.dat ($(wc -c <mrubyvm/mruby_gen.dat | tr -d ' ') bytes)"
