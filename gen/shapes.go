package gen

import (
	"fmt"
	"sort"
	"strings"
)

// --- leg 2: per-shape cgo trampoline collapse ---
//
// Stock cgo emits one Go wrapper + one .cgo2.c trampoline PER shim call (~32k of them), which
// dominates both the C-compile and the gc typecheck of the 851k-line _cgo_gotypes.go. But those
// trampolines are keyed by call SHAPE (the C signature), not by the specific shim: ~32k calls
// collapse onto ~1.2k distinct signatures. So instead of calling C.gen_X(args) directly, we route
// every shim call through a shape-generic dispatcher:
//
//	C.cppgo_shim_<shape>(C.gen_X, args...)
//
// C.gen_X is referenced as a VALUE (not called), which cgo lowers to an fpvar — a cheap
// `var _ unsafe.Pointer = &__cgo_gen_X` (4 trivial lines, no typed wrapper, no trampoline body) that
// still forces the symbol reference so the linker keeps it (no DCE drop). The dispatcher is a
// static-inline that does an ABI-identical indirect call (the shims are extern "C", so a correctly
// typed function-pointer cast is exactly the direct call). cgo then emits one wrapper+trampoline per
// distinct shape (~1.2k) instead of per call (~32k). Shapes are collected during emit and written to
// cppgo_shims.h by cmd/gen; the header is #included from every binding .go preamble.

type shimShape struct {
	ret    string   // C return type ("void" for void)
	params []string // ordered C parameter types, including the leading void* self when present
}

var shimShapes = map[string]shimShape{} // dispatcher name -> shape (process-global == package-scoped)

// Leg-2 shape oracle (step 4): one compile-time assertion per routed shim that the dispatcher's
// function-pointer cast (built from gen.go's shape logic) is type-compatible with the real shim
// prototype (emitted by the C++ side into gen_*.h). The C compiler reconciles the two independent
// derivations, so a shape bug fails the (verify-only) build naming the offending symbol — no runtime.
var (
	shimVerify     []string // _Static_assert lines, one per routed shim
	shimVerifyHdrs []string // headers to #include (the prototypes the asserts reference), dedup'd
	shimVerifySeen = map[string]bool{}
)

// cTypeOf strips the parameter name from a C parameter declaration: "const char* s" -> "const char*",
// "long long len" -> "long long", "void* self" -> "void*".
func cTypeOf(decl string) string {
	decl = strings.TrimSpace(decl)
	if i := strings.LastIndex(decl, " "); i >= 0 {
		return strings.TrimSpace(decl[:i])
	}
	return decl
}

// shimShapeTok turns a C type into a C-identifier-safe token for the dispatcher name
// ("const char*" -> "constcharp", "long long" -> "longlong", "void**" -> "voidpp").
func shimShapeTok(t string) string {
	return strings.NewReplacer(" ", "", "*", "p", "&", "r").Replace(t)
}

// registerShimShape records the C signature of m's shim (self iff a leading void* receiver is passed),
// emits its shape oracle assertion (header = the gen_*.h declaring m.cFunc), and returns the name of
// the dispatcher to route the call through.
func registerShimShape(self bool, m method, header string) string {
	var params []string
	if self {
		params = append(params, "void*")
	}
	for _, p := range m.params {
		for _, cp := range p.cParams {
			params = append(params, cTypeOf(cp))
		}
	}
	for _, ce := range m.ret.cExtra {
		params = append(params, cTypeOf(ce))
	}
	ret := m.ret.cRetType
	if ret == "" {
		ret = "void"
	}
	name := "cppgo_shim_" + shimShapeTok(ret)
	for _, p := range params {
		name += "_" + shimShapeTok(p)
	}
	if probing {
		// Fixpoint's renderability probe: the emitted source is discarded and the record may be pruned,
		// so registering its shapes/oracle lines would make cppgo_shims.h / cppgo_verify.c reference
		// headers that are never written. Return the (correct) dispatcher name without registering.
		return name
	}
	shimShapes[name] = shimShape{ret: ret, params: params}

	// Shape oracle: the dispatcher casts fn to `ret(*)(params)`, so that function type must be
	// compatible with the real shim. C spells "no params" as (void), not ().
	cParamList := "void"
	if len(params) > 0 {
		cParamList = strings.Join(params, ", ")
	}
	shimVerify = append(shimVerify, fmt.Sprintf(
		"_Static_assert(__builtin_types_compatible_p(__typeof__(%s), %s(%s)), \"cppgo leg2 shape mismatch: %s\");",
		m.cFunc, ret, cParamList, m.cFunc))
	if !shimVerifySeen[header] {
		shimVerifySeen[header] = true
		shimVerifyHdrs = append(shimVerifyHdrs, header)
	}
	return name
}

// EmittedShimHeader renders cppgo_shims.h: one static-inline dispatcher per registered shape. Each
// casts the passed function pointer to the shape's exact C signature and forwards the args, which is
// ABI-identical to a direct call (the shims are extern "C"). Returns "" when no shapes were collected.
func EmittedShimHeader() string {
	if len(shimShapes) == 0 {
		return ""
	}
	names := make([]string, 0, len(shimShapes))
	for n := range shimShapes {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("#pragma once\n// Code generated by cppgo; DO NOT EDIT. Per-shape cgo trampoline collapse (leg 2).\n\n")
	for _, n := range names {
		s := shimShapes[n]
		var decl, callArgs []string
		for i, p := range s.params {
			decl = append(decl, fmt.Sprintf("%s a%d", p, i))
			callArgs = append(callArgs, fmt.Sprintf("a%d", i))
		}
		sig := append([]string{"void* fn"}, decl...)
		fnType := fmt.Sprintf("%s(*)(%s)", s.ret, strings.Join(s.params, ", "))
		body := fmt.Sprintf("((%s)fn)(%s)", fnType, strings.Join(callArgs, ", "))
		fmt.Fprintf(&b, "static inline %s %s(%s) {\n", s.ret, n, strings.Join(sig, ", "))
		if s.ret == "void" {
			fmt.Fprintf(&b, "\t%s;\n}\n", body)
		} else {
			fmt.Fprintf(&b, "\treturn %s;\n}\n", body)
		}
	}
	return b.String()
}

// EmittedVerifyFile renders cppgo_verify.c: the leg-2 shape oracle. Compiled inert in normal builds;
// build with -DCPPGO_VERIFY (e.g. CGO_CFLAGS) to make the C compiler prove every shape dispatcher's
// fn-ptr cast is type-compatible with the real shim prototype. Returns "" when nothing was routed.
func EmittedVerifyFile() string {
	if len(shimVerify) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("// Code generated by cppgo; DO NOT EDIT. Leg-2 per-shape ABI oracle (step 4).\n")
	b.WriteString("// Inert unless built with -DCPPGO_VERIFY; then the C compiler proves every shape\n")
	b.WriteString("// dispatcher's fn-ptr cast == the real shim signature, naming any mismatch.\n")
	b.WriteString("#ifdef CPPGO_VERIFY\n")
	for _, h := range shimVerifyHdrs {
		fmt.Fprintf(&b, "#include %s\n", includeSpelling(h))
	}
	b.WriteString("\n")
	for _, a := range shimVerify {
		b.WriteString(a)
		b.WriteString("\n")
	}
	b.WriteString("#endif\n")
	return b.String()
}
