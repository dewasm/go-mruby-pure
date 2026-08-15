/*
** shim.c - the `dm_*` guest ABI go-mruby drives from Go.
**
** One wasm instance is one mrb_state, so no entry point takes a VM handle.
** Every pointer crossing the boundary is a guest address and every string is a (pointer, length) pair of raw bytes; the host must read a returned pointer before the next entry point runs, because the object behind it is only pinned until the next capture.
**
** Values the host holds live in a registry Array anchored in `root`, which is the single object registered with mrb_gc_register: reachability through `root` is what keeps host-held values, captured results, and in-flight argument vectors out of the collector's hands.
**
** Reentrancy: host_call arrives while the guest is inside dm_eval/dm_call, and the host may call dm_eval/dm_call/dm_yield again from inside host_call.
** Both the argument scratch and the host-argument area are therefore stacks of frames, not single buffers: an entry point consumes the top scratch frame and runs the guest with a fresh one pushed, and the trampoline pushes a host-argument frame for the duration of one host_call.
** Every entry point that can run Ruby code runs it under mrb_protect_error, so no exception ever unwinds through a host frame.
*/

#include <stdint.h>
#include <stdio.h>
#include <string.h>

#include <mruby.h>
#include <mruby/array.h>
#include <mruby/class.h>
#include <mruby/compile.h>
#include <mruby/error.h>
#include <mruby/hash.h>
#include <mruby/proc.h>
#include <mruby/string.h>
#include <mruby/value.h>
#include <mruby/variable.h>
#ifdef MRB_USE_BIGINT
/* mrb_bint_* is mruby's own internal header, not the public API: nothing else declares the bigint entry points the i64 ABI needs. */
#include <mruby/internal.h>
#endif

/* The ABI passes Float as f64, so mrb_float must be double in every configuration. */
_Static_assert(sizeof(mrb_float) == 8, "mrb_float must be double: do not build with MRB_USE_FLOAT32");

/* The ABI passes Integer as i64. Where mrb_int is narrower, mruby-bigint carries the rest of the range, so the gem is required there. */
#if !defined(MRB_INT64) && !defined(MRB_USE_BIGINT)
#error "an mrb_int narrower than 64-bit needs mruby-bigint: build the gem and define MRB_USE_BIGINT"
#endif

#define DM_EXPORT(name) __attribute__((export_name(#name), used)) name

__attribute__((import_module("host"), import_name("call"))) extern int32_t host_call(int32_t fn_id);

enum {
  DM_OK = 0,       /* the call completed; the result capture holds its value */
  DM_RAISED = 1,   /* a Ruby exception; the error capture holds it */
  DM_INTERNAL = 2  /* the ABI was misused (no dm_init, a stale ref, a wrong kind); the message accessors describe it */
};

enum {
  DM_KIND_NIL = 0,
  DM_KIND_FALSE = 1,
  DM_KIND_TRUE = 2,
  DM_KIND_INT = 3,
  DM_KIND_FLOAT = 4,
  DM_KIND_STRING = 5,
  DM_KIND_SYMBOL = 6,
  DM_KIND_ARRAY = 7,
  DM_KIND_HASH = 8,
  DM_KIND_OTHER = 9
};

enum {
  ROOT_REGISTRY = 0,    /* ref id -> value; index 0 is never handed out, so ref 0 reads as "none" everywhere */
  ROOT_SCRATCH,         /* stack of argument-vector frames */
  ROOT_HOSTARGS,        /* stack of [self, block, arg...] frames, one per in-flight host_call */
  ROOT_RESULT,
  ROOT_RESULT_STR,      /* the bytes dm_result_str hands out: the result itself for a String, its name for a Symbol, else nil until dm_result_to_s */
  ROOT_ERROR,
  ROOT_ERROR_CLASS,
  ROOT_ERROR_MESSAGE,
  ROOT_ERROR_BACKTRACE,
  ROOT_RAISE_CLASS,     /* what dm_host_raise recorded for the trampoline to raise */
  ROOT_RAISE_MESSAGE,
  ROOT_LEN
};

#define DM_SLOT_IN_USE (-2)
#define DM_SLOT_END (-1)

static mrb_state *mrb;
static mrb_value root;
static mrbc_context *eval_cxt;

static int32_t *slot_next;
static int32_t slot_cap;
static int32_t free_head = DM_SLOT_END;

static int32_t scratch_depth;
static int32_t hostargs_depth;

/* Set when an entry point fails without a Ruby exception; a static string, so the error accessors also answer before dm_init. */
static const char *internal_error;

static mrb_value
root_get(int slot)
{
  return mrb_ary_ref(mrb, root, slot);
}

static void
root_set(int slot, mrb_value v)
{
  mrb_ary_set(mrb, root, slot, v);
}

static int32_t
fail_internal(const char *msg)
{
  internal_error = msg;
  return DM_INTERNAL;
}

/* --- registry ------------------------------------------------------------ */

static mrb_bool
slots_reserve(int32_t n)
{
  if (n <= slot_cap) return TRUE;
  int32_t cap = slot_cap ? slot_cap : 16;
  while (cap < n) cap *= 2;
  int32_t *p = (int32_t*)mrb_realloc_simple(mrb, slot_next, (size_t)cap * sizeof(int32_t));
  if (!p) return FALSE;
  /* Reachable through reg_get before they are ever handed out, so a slot never reads as in use by accident. */
  for (int32_t i = slot_cap; i < cap; i++) {
    p[i] = DM_SLOT_END;
  }
  slot_next = p;
  slot_cap = cap;
  return TRUE;
}

static int32_t
reg_alloc(mrb_value v)
{
  mrb_value reg = root_get(ROOT_REGISTRY);
  int32_t idx;
  if (free_head != DM_SLOT_END) {
    idx = free_head;
    free_head = slot_next[idx];
  }
  else {
    idx = (int32_t)RARRAY_LEN(reg);
    if (!slots_reserve(idx + 1)) return 0;
  }
  slot_next[idx] = DM_SLOT_IN_USE;
  mrb_ary_set(mrb, reg, idx, v);
  return idx;
}

static mrb_bool
reg_get(int32_t id, mrb_value *out)
{
  if (!mrb || id <= 0 || id >= slot_cap || slot_next[id] != DM_SLOT_IN_USE) return FALSE;
  *out = mrb_ary_ref(mrb, root_get(ROOT_REGISTRY), id);
  return TRUE;
}

/* --- frame stacks -------------------------------------------------------- */

static mrb_value
frames_push(int slot, int32_t *depth)
{
  mrb_value stack = root_get(slot);
  mrb_value frame;
  if (*depth < (int32_t)RARRAY_LEN(stack)) {
    frame = mrb_ary_ref(mrb, stack, *depth);
    mrb_ary_resize(mrb, frame, 0);
  }
  else {
    frame = mrb_ary_new(mrb);
    mrb_ary_push(mrb, stack, frame);
  }
  (*depth)++;
  return frame;
}

static void
frames_pop(int slot, int32_t *depth)
{
  if (*depth <= 0) return;
  (*depth)--;
  mrb_ary_resize(mrb, mrb_ary_ref(mrb, root_get(slot), *depth), 0);
}

static mrb_value
frames_top(int slot, int32_t depth)
{
  if (depth <= 0) return mrb_nil_value();
  return mrb_ary_ref(mrb, root_get(slot), depth - 1);
}

/* --- integers ------------------------------------------------------------ */

/*
** Integer crosses the boundary as i64 whatever mrb_int is.
** Where mrb_int is narrower, the values between it and i64 cross as bigints, so every read goes through value_as_int64 and every write through int64_value.
** An Integer past i64 has no representation in the ABI: it reads as kind other, which is where a bigint already landed when mrb_int was 64-bit.
*/

#ifdef MRB_USE_BIGINT
typedef struct dm_bint {
  mrb_value v;
  int64_t out;
  mrb_bool ok;
} dm_bint;

/* The magnitude is read unsigned: mruby 4.0.0's mrb_bint_as_int64 rejects 2**63 before it looks at the sign, so it cannot read INT64_MIN. */
static mrb_bool
bint_to_int64(mrb_state *m, mrb_value v, int64_t *out)
{
  mrb_bool neg = mrb_bint_sign(m, v) < 0;
  uint64_t u = mrb_bint_as_uint64(m, neg ? mrb_bint_neg(m, v) : v);
  uint64_t limit = neg ? (uint64_t)INT64_MAX + 1 : (uint64_t)INT64_MAX;
  if (u > limit) return FALSE;
  *out = neg ? (u == (uint64_t)INT64_MAX + 1 ? INT64_MIN : -(int64_t)u) : (int64_t)u;
  return TRUE;
}

static mrb_value
body_bint_as_int64(mrb_state *m, void *ud)
{
  dm_bint *b = (dm_bint*)ud;
  b->ok = bint_to_int64(m, b->v, &b->out);
  return mrb_nil_value();
}

/*
** mrb_bint_as_uint64 raises past 2**64, and the accessors calling this run outside any protected frame.
** mrb_bint_size is the size of the limb array, an upper bound on the magnitude, so within eight bytes the conversion cannot raise and runs directly: that is every Integer the ABI can carry, and mrb_protect_error costs more than the conversion.
*/
static mrb_bool
bint_as_int64(mrb_value v, int64_t *out)
{
  if (mrb_bint_size(mrb, v) <= (mrb_int)sizeof(uint64_t)) return bint_to_int64(mrb, v, out);

  dm_bint b = {v, 0, FALSE};
  mrb_bool failed = FALSE;
  mrb_protect_error(mrb, body_bint_as_int64, &b, &failed);
  if (failed || !b.ok) return FALSE;
  *out = b.out;
  return TRUE;
}
#endif

static mrb_bool
value_as_int64(mrb_value v, int64_t *out)
{
  if (mrb_integer_p(v)) {
    *out = (int64_t)mrb_integer(v);
    return TRUE;
  }
#ifdef MRB_USE_BIGINT
  if (mrb_bigint_p(v)) return bint_as_int64(v, out);
#endif
  return FALSE;
}

static mrb_value
int64_value(int64_t v)
{
#ifdef MRB_INT64
  return mrb_int_value(mrb, (mrb_int)v);
#else
  if (v >= MRB_INT_MIN && v <= MRB_INT_MAX) return mrb_int_value(mrb, (mrb_int)v);
  /*
  ** The magnitude goes through mrb_bint_new_uint64 and gets its sign back afterwards, rather than through mrb_bint_new_int64.
  ** mruby 4.0.0's mrb_bint_new_int64 (mrbgems/mruby-bigint/core/bigint.c) hands an uninitialized mpz_t to mpz_realloc, which then writes the limbs through whatever pointer the stack held; the value reads back as 0.
  */
  mrb_value b = mrb_bint_new_uint64(mrb, v < 0 ? ~(uint64_t)v + 1 : (uint64_t)v);
  return v < 0 ? mrb_bint_neg(mrb, b) : b;
#endif
}

/* --- captures ------------------------------------------------------------ */

static int32_t
value_kind(mrb_value v)
{
  switch (mrb_type(v)) {
  case MRB_TT_FALSE: return mrb_nil_p(v) ? DM_KIND_NIL : DM_KIND_FALSE;
  case MRB_TT_TRUE: return DM_KIND_TRUE;
  case MRB_TT_INTEGER: return DM_KIND_INT;
  case MRB_TT_FLOAT: return DM_KIND_FLOAT;
  case MRB_TT_STRING: return DM_KIND_STRING;
  case MRB_TT_SYMBOL: return DM_KIND_SYMBOL;
  case MRB_TT_ARRAY: return DM_KIND_ARRAY;
  case MRB_TT_HASH: return DM_KIND_HASH;
  case MRB_TT_BIGINT: {
    int64_t n;
    return value_as_int64(v, &n) ? DM_KIND_INT : DM_KIND_OTHER;
  }
  default: return DM_KIND_OTHER;
  }
}

static void
capture_result(mrb_value v)
{
  internal_error = NULL;
  root_set(ROOT_RESULT, v);
  mrb_value s = mrb_nil_value();
  if (mrb_string_p(v)) {
    s = v;
  }
  else if (mrb_symbol_p(v)) {
    mrb_int len;
    const char *name = mrb_sym_name_len(mrb, mrb_symbol(v), &len);
    s = mrb_str_new(mrb, name, len);
  }
  root_set(ROOT_RESULT_STR, s);
}

static mrb_value
body_exc_message(mrb_state *m, void *ud)
{
  return mrb_obj_as_string(m, *(mrb_value*)ud);
}

static mrb_value
body_exc_backtrace(mrb_state *m, void *ud)
{
  mrb_value bt = mrb_funcall_argv(m, *(mrb_value*)ud, mrb_intern_lit(m, "backtrace"), 0, NULL);
  mrb_value out = mrb_str_new_lit(m, "");
  if (!mrb_array_p(bt)) return out;
  for (mrb_int i = 0; i < RARRAY_LEN(bt); i++) {
    if (i > 0) mrb_str_cat_lit(m, out, "\n");
    mrb_str_concat(m, out, mrb_obj_as_string(m, mrb_ary_ref(m, bt, i)));
  }
  return out;
}

static void
capture_error(mrb_value exc)
{
  internal_error = NULL;
  root_set(ROOT_ERROR, exc);
  root_set(ROOT_ERROR_CLASS, mrb_str_new_cstr(mrb, mrb_obj_classname(mrb, exc)));

  mrb_bool failed = FALSE;
  mrb_value msg = mrb_protect_error(mrb, body_exc_message, &exc, &failed);
  root_set(ROOT_ERROR_MESSAGE, (!failed && mrb_string_p(msg)) ? msg : mrb_str_new_lit(mrb, ""));

  failed = FALSE;
  mrb_value bt = mrb_protect_error(mrb, body_exc_backtrace, &exc, &failed);
  root_set(ROOT_ERROR_BACKTRACE, (!failed && mrb_string_p(bt)) ? bt : mrb_str_new_lit(mrb, ""));
}

/*
** Run one guest operation with a fresh scratch frame on top, catching every Ruby exception.
** mrb_load_* reports a syntax error by setting mrb->exc instead of raising, so a pending exception is checked on the success path too.
*/
static int32_t
protected_run(mrb_protect_error_func *body, void *ud)
{
  if (!mrb) return fail_internal("dm_init has not run");
  internal_error = NULL;
  mrb_bool failed = FALSE;
  frames_push(ROOT_SCRATCH, &scratch_depth);
  mrb_value v = mrb_protect_error(mrb, body, ud, &failed);
  frames_pop(ROOT_SCRATCH, &scratch_depth);
  if (failed) {
    capture_error(v);
    return DM_RAISED;
  }
  if (mrb->exc) {
    mrb_value exc = mrb_obj_value(mrb->exc);
    mrb->exc = NULL;
    capture_error(exc);
    return DM_RAISED;
  }
  capture_result(v);
  return DM_OK;
}

/* --- the host trampoline ------------------------------------------------- */

static struct RClass*
lookup_class(mrb_value name)
{
  if (!mrb_string_p(name) || RSTRING_LEN(name) == 0) return NULL;
  const char *p = RSTRING_PTR(name);
  mrb_int len = RSTRING_LEN(name);
  mrb_value mod = mrb_obj_value(mrb->object_class);
  mrb_int start = 0;
  for (;;) {
    mrb_int end = start;
    while (end < len && !(p[end] == ':' && end + 1 < len && p[end + 1] == ':')) end++;
    if (end == start) return NULL;
    mrb_sym sym = mrb_intern(mrb, p + start, end - start);
    if (!mrb_const_defined(mrb, mod, sym)) return NULL;
    mod = mrb_const_get(mrb, mod, sym);
    if (mrb_type(mod) != MRB_TT_CLASS && mrb_type(mod) != MRB_TT_MODULE) return NULL;
    if (end >= len) break;
    start = end + 2;
  }
  return mrb_class_ptr(mod);
}

static mrb_value
dm_trampoline(mrb_state *m, mrb_value self)
{
  mrb_value *argv;
  mrb_int argc;
  mrb_value block;
  mrb_get_args(m, "*&", &argv, &argc, &block);
  mrb_int fn_id = mrb_integer(mrb_proc_cfunc_env_get(m, 0));

  int ai = mrb_gc_arena_save(m);
  mrb_value frame = frames_push(ROOT_HOSTARGS, &hostargs_depth);
  mrb_ary_push(m, frame, self);
  mrb_ary_push(m, frame, block);
  for (mrb_int i = 0; i < argc; i++) {
    mrb_ary_push(m, frame, argv[i]);
  }
  frames_push(ROOT_SCRATCH, &scratch_depth);
  root_set(ROOT_RAISE_CLASS, mrb_nil_value());
  root_set(ROOT_RAISE_MESSAGE, mrb_nil_value());

  int32_t rc = host_call((int32_t)fn_id);

  /* The host writes its return value with dm_args_reset + one dm_args_push_*, so the scratch top's first element is it. */
  mrb_value ret = mrb_nil_value();
  if (rc == 0) {
    mrb_value scratch = frames_top(ROOT_SCRATCH, scratch_depth);
    if (mrb_array_p(scratch) && RARRAY_LEN(scratch) > 0) ret = mrb_ary_ref(m, scratch, 0);
  }
  mrb_value raise_class = root_get(ROOT_RAISE_CLASS);
  mrb_value raise_message = root_get(ROOT_RAISE_MESSAGE);
  frames_pop(ROOT_SCRATCH, &scratch_depth);
  frames_pop(ROOT_HOSTARGS, &hostargs_depth);

  if (rc != 0) {
    struct RClass *c = lookup_class(raise_class);
    if (!c) c = mrb_class_get(m, "RuntimeError");
    mrb_value msg = mrb_string_p(raise_message) ? raise_message : mrb_str_new_lit(m, "host error");
    mrb_exc_raise(m, mrb_exc_new_str(m, c, msg));
  }

  mrb_gc_arena_restore(m, ai);
  if (!mrb_immediate_p(ret)) mrb_gc_protect(m, ret);
  return ret;
}

/* --- lifecycle ----------------------------------------------------------- */

int32_t
DM_EXPORT(dm_init)(void)
{
  if (mrb) return DM_OK;
  mrb = mrb_open();
  if (!mrb) return fail_internal("mrb_open failed");

  /* Kernel#print flushes only on a tty (src/print.c), and this WASI is not one, so unbuffered stdio is what makes host-side output capture immediate. */
  setvbuf(stdout, NULL, _IONBF, 0);
  setvbuf(stderr, NULL, _IONBF, 0);

  root = mrb_ary_new_capa(mrb, ROOT_LEN);
  mrb_gc_register(mrb, root);
  for (int i = 0; i < ROOT_LEN; i++) {
    mrb_ary_set(mrb, root, i, mrb_nil_value());
  }
  mrb_value registry = mrb_ary_new(mrb);
  mrb_ary_push(mrb, registry, mrb_nil_value());
  root_set(ROOT_REGISTRY, registry);
  root_set(ROOT_SCRATCH, mrb_ary_new(mrb));
  root_set(ROOT_HOSTARGS, mrb_ary_new(mrb));
  if (!slots_reserve(1)) return fail_internal("out of memory");
  slot_next[0] = DM_SLOT_IN_USE;

  /* The base scratch frame: the host builds arguments in a frame at all times, including outside any call. */
  frames_push(ROOT_SCRATCH, &scratch_depth);

  eval_cxt = mrbc_context_new(mrb);
  if (!eval_cxt) return fail_internal("mrbc_context_new failed");
  mrbc_filename(mrb, eval_cxt, "(eval)");
  eval_cxt->capture_errors = TRUE;
  return DM_OK;
}

uint32_t
DM_EXPORT(dm_alloc)(uint32_t size)
{
  if (!mrb) return 0;
  return (uint32_t)(uintptr_t)mrb_malloc_simple(mrb, size ? size : 1);
}

void
DM_EXPORT(dm_free)(uint32_t ptr)
{
  if (!mrb || ptr == 0) return;
  mrb_free(mrb, (void*)(uintptr_t)ptr);
}

/* --- eval and calls ------------------------------------------------------ */

typedef struct dm_op {
  const char *ptr;
  mrb_int len;
  mrb_value args;
  int32_t recv_ref;
  int32_t block_ref;
  mrb_int index;
} dm_op;

static mrb_value
body_eval(mrb_state *m, void *ud)
{
  dm_op *op = (dm_op*)ud;
  return mrb_load_nstring_cxt(m, op->ptr, op->len, eval_cxt);
}

int32_t
DM_EXPORT(dm_eval)(uint32_t src, uint32_t len)
{
  dm_op op = {(const char*)(uintptr_t)src, (mrb_int)len, mrb_nil_value(), 0, 0, 0};
  return protected_run(body_eval, &op);
}

static mrb_bool
op_receiver(dm_op *op, mrb_value *out)
{
  if (op->recv_ref == 0) {
    *out = mrb_top_self(mrb);
    return TRUE;
  }
  return reg_get(op->recv_ref, out);
}

static mrb_value
body_call(mrb_state *m, void *ud)
{
  dm_op *op = (dm_op*)ud;
  mrb_value recv;
  op_receiver(op, &recv);
  mrb_sym mid = mrb_intern(m, op->ptr, op->len);
  mrb_int argc = RARRAY_LEN(op->args);
  mrb_value *argv = RARRAY_PTR(op->args);
  if (op->block_ref < 0) return mrb_funcall_argv(m, recv, mid, argc, argv);
  mrb_value block = mrb_nil_value();
  reg_get(op->block_ref, &block);
  return mrb_funcall_with_block(m, recv, mid, argc, argv, block);
}

static mrb_value
body_yield(mrb_state *m, void *ud)
{
  dm_op *op = (dm_op*)ud;
  mrb_value proc;
  reg_get(op->block_ref, &proc);
  return mrb_yield_argv(m, proc, (mrb_int)RARRAY_LEN(op->args), RARRAY_PTR(op->args));
}

/* The scratch frame the host filled becomes the argument vector; protected_run pushes a fresh frame over it, so a host callback reached from this call cannot disturb it. */
static mrb_value
consume_args(void)
{
  return frames_top(ROOT_SCRATCH, scratch_depth);
}

/* One call consumes the arguments, so the next one starts empty whether or not the host resets the scratch itself. */
static int32_t
finish_call(int32_t status, mrb_value args)
{
  if (mrb_array_p(args)) mrb_ary_resize(mrb, args, 0);
  return status;
}

int32_t
DM_EXPORT(dm_call)(int32_t recv_ref, uint32_t name, uint32_t name_len)
{
  if (!mrb) return fail_internal("dm_init has not run");
  dm_op op = {(const char*)(uintptr_t)name, (mrb_int)name_len, consume_args(), recv_ref, -1, 0};
  mrb_value recv;
  if (!op_receiver(&op, &recv)) return fail_internal("dm_call: unknown receiver ref");
  return finish_call(protected_run(body_call, &op), op.args);
}

int32_t
DM_EXPORT(dm_call_block)(int32_t recv_ref, uint32_t name, uint32_t name_len, int32_t block_ref)
{
  if (!mrb) return fail_internal("dm_init has not run");
  dm_op op = {(const char*)(uintptr_t)name, (mrb_int)name_len, consume_args(), recv_ref, block_ref, 0};
  mrb_value recv;
  if (!op_receiver(&op, &recv)) return fail_internal("dm_call_block: unknown receiver ref");
  mrb_value block;
  if (block_ref != 0 && !reg_get(block_ref, &block)) return fail_internal("dm_call_block: unknown block ref");
  return finish_call(protected_run(body_call, &op), op.args);
}

int32_t
DM_EXPORT(dm_yield)(int32_t proc_ref)
{
  if (!mrb) return fail_internal("dm_init has not run");
  mrb_value proc;
  if (!reg_get(proc_ref, &proc)) return fail_internal("dm_yield: unknown proc ref");
  if (mrb_type(proc) != MRB_TT_PROC) return fail_internal("dm_yield: ref is not a Proc");
  dm_op op = {NULL, 0, consume_args(), 0, proc_ref, 0};
  return finish_call(protected_run(body_yield, &op), op.args);
}

/* --- result accessors ---------------------------------------------------- */

static uint32_t
str_ptr(mrb_value s)
{
  return mrb_string_p(s) ? (uint32_t)(uintptr_t)RSTRING_PTR(s) : 0;
}

static int32_t
str_len(mrb_value s)
{
  return mrb_string_p(s) ? (int32_t)RSTRING_LEN(s) : 0;
}

int32_t
DM_EXPORT(dm_result_kind)(void)
{
  if (!mrb) return DM_KIND_NIL;
  return value_kind(root_get(ROOT_RESULT));
}

int64_t
DM_EXPORT(dm_result_int)(void)
{
  if (!mrb) return 0;
  int64_t n = 0;
  value_as_int64(root_get(ROOT_RESULT), &n);
  return n;
}

double
DM_EXPORT(dm_result_float)(void)
{
  if (!mrb) return 0;
  mrb_value v = root_get(ROOT_RESULT);
  return mrb_float_p(v) ? (double)mrb_float(v) : 0;
}

uint32_t
DM_EXPORT(dm_result_str)(void)
{
  if (!mrb) return 0;
  return str_ptr(root_get(ROOT_RESULT_STR));
}

int32_t
DM_EXPORT(dm_result_str_len)(void)
{
  if (!mrb) return 0;
  return str_len(root_get(ROOT_RESULT_STR));
}

static mrb_value
body_result_to_s(mrb_state *m, void *ud)
{
  (void)ud;
  return mrb_obj_as_string(m, root_get(ROOT_RESULT));
}

int32_t
DM_EXPORT(dm_result_to_s)(void)
{
  if (!mrb) return fail_internal("dm_init has not run");
  internal_error = NULL;
  mrb_bool failed = FALSE;
  frames_push(ROOT_SCRATCH, &scratch_depth);
  mrb_value s = mrb_protect_error(mrb, body_result_to_s, NULL, &failed);
  frames_pop(ROOT_SCRATCH, &scratch_depth);
  if (failed) {
    capture_error(s);
    return DM_RAISED;
  }
  root_set(ROOT_RESULT_STR, s);
  return DM_OK;
}

int32_t
DM_EXPORT(dm_result_ref)(void)
{
  if (!mrb) return 0;
  return reg_alloc(root_get(ROOT_RESULT));
}

/* --- error accessors ----------------------------------------------------- */

uint32_t
DM_EXPORT(dm_error_class)(void)
{
  if (internal_error || !mrb) return 0;
  return str_ptr(root_get(ROOT_ERROR_CLASS));
}

int32_t
DM_EXPORT(dm_error_class_len)(void)
{
  if (internal_error || !mrb) return 0;
  return str_len(root_get(ROOT_ERROR_CLASS));
}

uint32_t
DM_EXPORT(dm_error_message)(void)
{
  if (internal_error) return (uint32_t)(uintptr_t)internal_error;
  if (!mrb) return 0;
  return str_ptr(root_get(ROOT_ERROR_MESSAGE));
}

int32_t
DM_EXPORT(dm_error_message_len)(void)
{
  if (internal_error) return (int32_t)strlen(internal_error);
  if (!mrb) return 0;
  return str_len(root_get(ROOT_ERROR_MESSAGE));
}

uint32_t
DM_EXPORT(dm_error_backtrace)(void)
{
  if (internal_error || !mrb) return 0;
  return str_ptr(root_get(ROOT_ERROR_BACKTRACE));
}

int32_t
DM_EXPORT(dm_error_backtrace_len)(void)
{
  if (internal_error || !mrb) return 0;
  return str_len(root_get(ROOT_ERROR_BACKTRACE));
}

int32_t
DM_EXPORT(dm_error_ref)(void)
{
  if (internal_error || !mrb) return 0;
  mrb_value exc = root_get(ROOT_ERROR);
  if (mrb_nil_p(exc)) return 0;
  return reg_alloc(exc);
}

/* --- registry ------------------------------------------------------------ */

void
DM_EXPORT(dm_ref_release)(int32_t id)
{
  if (!mrb || id <= 0 || id >= slot_cap || slot_next[id] != DM_SLOT_IN_USE) return;
  mrb_ary_set(mrb, root_get(ROOT_REGISTRY), id, mrb_nil_value());
  slot_next[id] = free_head;
  free_head = id;
}

int32_t
DM_EXPORT(dm_kind)(int32_t id)
{
  mrb_value v;
  if (!reg_get(id, &v)) return -1;
  return value_kind(v);
}

/* The registry's high-water mark: refs are slot indices and released slots are reused, so this is what a leak test watches. */
int32_t
DM_EXPORT(dm_registry_len)(void)
{
  if (!mrb) return 0;
  return (int32_t)RARRAY_LEN(root_get(ROOT_REGISTRY));
}

/*
** Ruby object identity, read from C instead of by sending object_id, so that a Ruby-level override of object_id or __id__ cannot change what the host sees.
** An unknown ref answers 0, which mrb_obj_id gives no live object.
*/
int64_t
DM_EXPORT(dm_obj_id)(int32_t id)
{
  mrb_value v;
  if (!reg_get(id, &v)) return 0;
  return (int64_t)mrb_obj_id(v);
}

/* --- argument scratch ---------------------------------------------------- */

static void
args_push(mrb_value v)
{
  if (!mrb || scratch_depth <= 0) return;
  mrb_ary_push(mrb, frames_top(ROOT_SCRATCH, scratch_depth), v);
}

void
DM_EXPORT(dm_args_reset)(void)
{
  if (!mrb || scratch_depth <= 0) return;
  mrb_ary_resize(mrb, frames_top(ROOT_SCRATCH, scratch_depth), 0);
}

void
DM_EXPORT(dm_args_push_nil)(void)
{
  args_push(mrb_nil_value());
}

void
DM_EXPORT(dm_args_push_bool)(int32_t v)
{
  args_push(mrb_bool_value(v != 0));
}

void
DM_EXPORT(dm_args_push_int)(int64_t v)
{
  if (!mrb) return;
  args_push(int64_value(v));
}

void
DM_EXPORT(dm_args_push_float)(double v)
{
  if (!mrb) return;
  args_push(mrb_float_value(mrb, (mrb_float)v));
}

void
DM_EXPORT(dm_args_push_str)(uint32_t ptr, uint32_t len)
{
  if (!mrb) return;
  args_push(mrb_str_new(mrb, (const char*)(uintptr_t)ptr, (mrb_int)len));
}

void
DM_EXPORT(dm_args_push_sym)(uint32_t ptr, uint32_t len)
{
  if (!mrb) return;
  args_push(mrb_symbol_value(mrb_intern(mrb, (const char*)(uintptr_t)ptr, (mrb_int)len)));
}

void
DM_EXPORT(dm_args_push_ref)(int32_t id)
{
  mrb_value v = mrb_nil_value();
  reg_get(id, &v);
  args_push(v);
}

/* --- values built from the scratch --------------------------------------- */

/* The scratch frame's one value becomes the result capture: it is how an immediate gets a ref of its own and how a ref reads back as an immediate. */
int32_t
DM_EXPORT(dm_capture_arg)(void)
{
  if (!mrb) return fail_internal("dm_init has not run");
  mrb_value args = consume_args();
  if (!mrb_array_p(args) || RARRAY_LEN(args) != 1) {
    return fail_internal("dm_capture_arg: the scratch does not hold exactly one value");
  }
  capture_result(mrb_ary_ref(mrb, args, 0));
  mrb_ary_resize(mrb, args, 0);
  return DM_OK;
}

static mrb_value
body_new_array(mrb_state *m, void *ud)
{
  dm_op *op = (dm_op*)ud;
  return mrb_ary_new_from_values(m, RARRAY_LEN(op->args), RARRAY_PTR(op->args));
}

/* The whole scratch frame becomes an Array. */
int32_t
DM_EXPORT(dm_new_array)(void)
{
  if (!mrb) return fail_internal("dm_init has not run");
  dm_op op = {NULL, 0, consume_args(), 0, 0, 0};
  return finish_call(protected_run(body_new_array, &op), op.args);
}

static mrb_value
body_new_hash(mrb_state *m, void *ud)
{
  dm_op *op = (dm_op*)ud;
  mrb_int n = RARRAY_LEN(op->args);
  mrb_value hash = mrb_hash_new_capa(m, n / 2);
  for (mrb_int i = 0; i < n; i += 2) {
    mrb_hash_set(m, hash, mrb_ary_ref(m, op->args, i), mrb_ary_ref(m, op->args, i + 1));
  }
  return hash;
}

/* The whole scratch frame, taken as key/value pairs, becomes a Hash. */
int32_t
DM_EXPORT(dm_new_hash)(void)
{
  if (!mrb) return fail_internal("dm_init has not run");
  mrb_value args = consume_args();
  if (!mrb_array_p(args) || RARRAY_LEN(args) % 2 != 0) {
    return fail_internal("dm_new_hash: the scratch does not hold key/value pairs");
  }
  dm_op op = {NULL, 0, args, 0, 0, 0};
  return finish_call(protected_run(body_new_hash, &op), op.args);
}

/* --- structure access ---------------------------------------------------- */

int64_t
DM_EXPORT(dm_ary_len)(int32_t id)
{
  mrb_value v;
  if (!reg_get(id, &v) || !mrb_array_p(v)) return -1;
  return (int64_t)RARRAY_LEN(v);
}

static mrb_value
body_ary_get(mrb_state *m, void *ud)
{
  dm_op *op = (dm_op*)ud;
  mrb_value ary;
  reg_get(op->recv_ref, &ary);
  return mrb_ary_ref(m, ary, op->index);
}

int32_t
DM_EXPORT(dm_ary_get)(int32_t id, int64_t index)
{
  mrb_value v;
  if (!reg_get(id, &v)) return fail_internal("dm_ary_get: unknown ref");
  if (!mrb_array_p(v)) return fail_internal("dm_ary_get: ref is not an Array");
#ifndef MRB_INT64
  /* An index past mrb_int addresses no element, and Ruby reads any index out of range as nil. */
  if (index < MRB_INT_MIN || index > MRB_INT_MAX) {
    internal_error = NULL;
    capture_result(mrb_nil_value());
    return DM_OK;
  }
#endif
  dm_op op = {NULL, 0, mrb_nil_value(), id, 0, (mrb_int)index};
  return protected_run(body_ary_get, &op);
}

static mrb_value
body_hash_keys(mrb_state *m, void *ud)
{
  dm_op *op = (dm_op*)ud;
  mrb_value hash;
  reg_get(op->recv_ref, &hash);
  return mrb_hash_keys(m, hash);
}

int32_t
DM_EXPORT(dm_hash_keys)(int32_t id)
{
  mrb_value v;
  if (!reg_get(id, &v) || !mrb_hash_p(v)) {
    fail_internal("dm_hash_keys: ref is not a Hash");
    return 0;
  }
  dm_op op = {NULL, 0, mrb_nil_value(), id, 0, 0};
  if (protected_run(body_hash_keys, &op) != DM_OK) return 0;
  return reg_alloc(root_get(ROOT_RESULT));
}

static mrb_value
body_hash_get(mrb_state *m, void *ud)
{
  dm_op *op = (dm_op*)ud;
  mrb_value hash, key;
  reg_get(op->recv_ref, &hash);
  reg_get(op->block_ref, &key);
  return mrb_hash_get(m, hash, key);
}

int32_t
DM_EXPORT(dm_hash_get)(int32_t id, int32_t key_ref)
{
  mrb_value v, key;
  if (!reg_get(id, &v) || !mrb_hash_p(v)) return fail_internal("dm_hash_get: ref is not a Hash");
  if (!reg_get(key_ref, &key)) return fail_internal("dm_hash_get: unknown key ref");
  dm_op op = {NULL, 0, mrb_nil_value(), id, key_ref, 0};
  return protected_run(body_hash_get, &op);
}

/* --- definitions --------------------------------------------------------- */

typedef struct dm_def {
  const char *name;
  mrb_int name_len;
  int32_t target_ref;
  int32_t fn_id;
  mrb_bool module_function;
} dm_def;

static mrb_value
body_define_class(mrb_state *m, void *ud)
{
  dm_def *def = (dm_def*)ud;
  struct RClass *super = m->object_class;
  if (def->target_ref != 0) {
    mrb_value v;
    reg_get(def->target_ref, &v);
    super = mrb_class_ptr(v);
  }
  return mrb_obj_value(mrb_define_class_id(m, mrb_intern(m, def->name, def->name_len), super));
}

int32_t
DM_EXPORT(dm_define_class)(uint32_t name, uint32_t name_len, int32_t super_ref)
{
  if (!mrb) return fail_internal("dm_init has not run");
  if (super_ref != 0) {
    mrb_value v;
    if (!reg_get(super_ref, &v)) return fail_internal("dm_define_class: unknown superclass ref");
    if (mrb_type(v) != MRB_TT_CLASS) return fail_internal("dm_define_class: superclass ref is not a Class");
  }
  dm_def def = {(const char*)(uintptr_t)name, (mrb_int)name_len, super_ref, 0, FALSE};
  return protected_run(body_define_class, &def);
}

static mrb_value
body_define_method(mrb_state *m, void *ud)
{
  dm_def *def = (dm_def*)ud;
  struct RClass *c = m->kernel_module;
  if (def->target_ref != 0) {
    mrb_value v;
    reg_get(def->target_ref, &v);
    c = mrb_class_ptr(v);
  }
  mrb_value env = mrb_int_value(m, def->fn_id);
  struct RProc *p = mrb_proc_new_cfunc_with_env(m, dm_trampoline, 1, &env);
  mrb_method_t method;
  MRB_METHOD_FROM_PROC(method, p);
  mrb_sym mid = mrb_intern(m, def->name, def->name_len);
  if (def->module_function) {
    mrb_define_method_raw(m, mrb_singleton_class_ptr(m, mrb_obj_value(c)), mid, method);
  }
  mrb_define_method_raw(m, c, mid, method);
  return mrb_symbol_value(mid);
}

static int32_t
define_method_common(uint32_t name, uint32_t name_len, int32_t target_ref, int32_t fn_id, mrb_bool module_function)
{
  if (!mrb) return fail_internal("dm_init has not run");
  if (target_ref != 0) {
    mrb_value v;
    if (!reg_get(target_ref, &v)) return fail_internal("dm_define_method: unknown target ref");
    if (mrb_type(v) != MRB_TT_CLASS && mrb_type(v) != MRB_TT_MODULE && mrb_type(v) != MRB_TT_SCLASS) {
      return fail_internal("dm_define_method: target ref is not a Class or Module");
    }
  }
  dm_def def = {(const char*)(uintptr_t)name, (mrb_int)name_len, target_ref, fn_id, module_function};
  return protected_run(body_define_method, &def);
}

int32_t
DM_EXPORT(dm_define_method)(int32_t target_ref, uint32_t name, uint32_t name_len, int32_t fn_id)
{
  return define_method_common(name, name_len, target_ref, fn_id, FALSE);
}

int32_t
DM_EXPORT(dm_define_module_function)(int32_t target_ref, uint32_t name, uint32_t name_len, int32_t fn_id)
{
  return define_method_common(name, name_len, target_ref, fn_id, TRUE);
}

/* --- host arguments ------------------------------------------------------ */

static mrb_bool
hostarg(int32_t i, mrb_value *out)
{
  mrb_value frame = frames_top(ROOT_HOSTARGS, hostargs_depth);
  if (!mrb_array_p(frame) || i < 0 || i + 2 >= (int32_t)RARRAY_LEN(frame)) return FALSE;
  *out = mrb_ary_ref(mrb, frame, i + 2);
  return TRUE;
}

int32_t
DM_EXPORT(dm_hostargs_count)(void)
{
  if (!mrb) return 0;
  mrb_value frame = frames_top(ROOT_HOSTARGS, hostargs_depth);
  if (!mrb_array_p(frame)) return 0;
  return (int32_t)RARRAY_LEN(frame) - 2;
}

int32_t
DM_EXPORT(dm_hostargs_kind)(int32_t i)
{
  mrb_value v;
  if (!hostarg(i, &v)) return -1;
  return value_kind(v);
}

int64_t
DM_EXPORT(dm_hostargs_int)(int32_t i)
{
  mrb_value v;
  int64_t n = 0;
  if (!hostarg(i, &v)) return 0;
  value_as_int64(v, &n);
  return n;
}

double
DM_EXPORT(dm_hostargs_float)(int32_t i)
{
  mrb_value v;
  if (!hostarg(i, &v) || !mrb_float_p(v)) return 0;
  return (double)mrb_float(v);
}

/* A String's own bytes, a Symbol's name in the symbol table: both stay put while the call is in flight, so no copy is made. */
static const char*
hostarg_str(int32_t i, mrb_int *len)
{
  mrb_value v;
  *len = 0;
  if (!hostarg(i, &v)) return NULL;
  if (mrb_string_p(v)) {
    *len = RSTRING_LEN(v);
    return RSTRING_PTR(v);
  }
  if (mrb_symbol_p(v)) return mrb_sym_name_len(mrb, mrb_symbol(v), len);
  return NULL;
}

uint32_t
DM_EXPORT(dm_hostargs_str)(int32_t i)
{
  if (!mrb) return 0;
  mrb_int len;
  return (uint32_t)(uintptr_t)hostarg_str(i, &len);
}

int32_t
DM_EXPORT(dm_hostargs_str_len)(int32_t i)
{
  if (!mrb) return 0;
  mrb_int len;
  hostarg_str(i, &len);
  return (int32_t)len;
}

int32_t
DM_EXPORT(dm_hostargs_ref)(int32_t i)
{
  mrb_value v;
  if (!hostarg(i, &v)) return 0;
  return reg_alloc(v);
}

int32_t
DM_EXPORT(dm_hostargs_self)(void)
{
  if (!mrb) return 0;
  mrb_value frame = frames_top(ROOT_HOSTARGS, hostargs_depth);
  if (!mrb_array_p(frame) || RARRAY_LEN(frame) < 2) return 0;
  return reg_alloc(mrb_ary_ref(mrb, frame, 0));
}

int32_t
DM_EXPORT(dm_hostargs_block)(void)
{
  if (!mrb) return 0;
  mrb_value frame = frames_top(ROOT_HOSTARGS, hostargs_depth);
  if (!mrb_array_p(frame) || RARRAY_LEN(frame) < 2) return 0;
  mrb_value block = mrb_ary_ref(mrb, frame, 1);
  if (mrb_nil_p(block)) return 0;
  return reg_alloc(block);
}

void
DM_EXPORT(dm_host_raise)(uint32_t class_name, uint32_t class_len, uint32_t message, uint32_t message_len)
{
  if (!mrb) return;
  root_set(ROOT_RAISE_CLASS, mrb_str_new(mrb, (const char*)(uintptr_t)class_name, (mrb_int)class_len));
  root_set(ROOT_RAISE_MESSAGE, mrb_str_new(mrb, (const char*)(uintptr_t)message, (mrb_int)message_len));
}
