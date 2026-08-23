MRUBY_VERSION := 4.0.0
MRUBY_URL := https://github.com/mruby/mruby/archive/refs/tags/$(MRUBY_VERSION).tar.gz
MRUBY_SHA256 := e2ea271dbed14e9f2b33df773ae447b747dbc242ce2675022c0a57efea85a7b4

PROFILE ?= word
OPT ?= -O2
DEWASM_BIN ?= ../dewasm/target/release/dewasm
WASM_OPT ?= 0
STACK_SIZE := 1048576
WASM_OPT_FLAGS := \
  -O2 \
  --enable-exception-handling --enable-bulk-memory --enable-sign-ext \
  --enable-nontrapping-float-to-int --enable-mutable-globals --enable-multivalue \
  --enable-reference-types

CACHE := wasm/cache
TARBALL := $(CACHE)/mruby-$(MRUBY_VERSION).tar.gz
SRC := $(CACHE)/mruby-$(MRUBY_VERSION)
BUILD := $(CACHE)/build-$(PROFILE)/wasm32-wasi
LIB := $(BUILD)/lib/libmruby.a
SHIM := $(BUILD)/shim.o
RT := $(CACHE)/rt.o

.PHONY: all mruby-rake clean

all: mrubyvm/mruby_gen.go

$(TARBALL):
	mkdir -p $(CACHE)
	curl -fsSL --retry 5 --retry-delay 2 --retry-connrefused -o $@ $(MRUBY_URL)
	echo "$(MRUBY_SHA256)  $@" | shasum -a 256 -c - >/dev/null

$(SRC)/Rakefile: $(TARBALL)
	tar xzf $(TARBALL) -C $(CACHE)
	touch $@

$(RT): tools/mruby-cc.sh
	mkdir -p $(CACHE)
	rt_c="$$(zig env | sed -n 's/.*\.lib_dir = "\(.*\)",/\1/p')/libc/wasi/libc-top-half/musl/src/setjmp/wasm32/rt.c"; \
	  test -f "$$rt_c" || { echo "rt.c not found at $$rt_c (zig layout changed?)" >&2; exit 1; }; \
	  OPT='$(OPT)' tools/mruby-cc.sh -c -o $@ "$$rt_c"

mruby-rake: $(SRC)/Rakefile
	cd $(SRC) && \
	  MRUBY_CONFIG=$(abspath wasm/build_config.rb) MRUBY_BUILD_DIR=$(abspath $(CACHE)/build-$(PROFILE)) PROFILE=$(PROFILE) OPT='$(OPT)' \
	  rake

$(LIB) $(SHIM): | mruby-rake ;

wasm/mruby.wasm: $(SHIM) $(RT) $(LIB)
	zig cc -target wasm32-wasi -mexec-model=reactor -Wl,--strip-debug -Wl,-z,stack-size=$(STACK_SIZE) -o $@ $(SHIM) $(RT) $(LIB)
ifneq ($(WASM_OPT),0)
	wasm-opt $(WASM_OPT_FLAGS) $@ -o $@.opt
	mv $@.opt $@
endif

mrubyvm/mruby_gen.go: wasm/mruby.wasm
	test -x $(DEWASM_BIN) || { echo "dewasm binary not found at $(DEWASM_BIN); set DEWASM_BIN" >&2; exit 1; }
	$(DEWASM_BIN) wasm/mruby.wasm --target go --mode library --module-name Mrubyvm \
	  --data-file mrubyvm/mruby_gen.dat -o $@
	gofmt -w $@
	go vet ./mrubyvm

mrubyvm/mruby_gen.dat: mrubyvm/mruby_gen.go ;

clean:
	rm -rf $(CACHE) wasm/mruby.wasm
