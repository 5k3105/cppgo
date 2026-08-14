package gen

import (
	"strings"
	"testing"
)

// Validates the leg-2 shape-collapse rendering (registerShimShape + EmittedShimHeader) without any
// Qt parse/regen: synthetic methods exercise self/no-self, void/non-void, and multi-param shapes.
func TestShimShapeRendering(t *testing.T) {
	shimShapes = map[string]shimShape{} // isolate from any other test
	shimVerify, shimVerifyHdrs, shimVerifySeen = nil, nil, map[string]bool{}

	// QString::size() — instance, int(void*).
	size := method{cFunc: "gen_QString_size", ret: retPlan{cRetType: "int"}}
	// QWidget::resize(int,int) — instance, void(void*,int,int).
	resize := method{cFunc: "gen_QWidget_resize", ret: retPlan{isVoid: true, cRetType: "void"},
		params: []paramPlan{{cParams: []string{"int w"}}, {cParams: []string{"int h"}}}}
	// QString::fromUtf8(const char*) — static, void*(const char*).
	fromUtf8 := method{cFunc: "gen_QString_fromUtf8", isStat: true, ret: retPlan{cRetType: "void*"},
		params: []paramPlan{{cParams: []string{"const char* s"}}}}

	if got := registerShimShape(true, size, "gen_qstring.h"); got != "cppgo_shim_int_voidp" {
		t.Errorf("size shape = %q, want cppgo_shim_int_voidp", got)
	}
	if got := registerShimShape(true, resize, "gen_qwidget.h"); got != "cppgo_shim_void_voidp_int_int" {
		t.Errorf("resize shape = %q, want cppgo_shim_void_voidp_int_int", got)
	}
	if got := registerShimShape(false, fromUtf8, "gen_qstring.h"); got != "cppgo_shim_voidp_constcharp" {
		t.Errorf("fromUtf8 shape = %q, want cppgo_shim_voidp_constcharp", got)
	}

	h := EmittedShimHeader()
	want := []string{
		"static inline int cppgo_shim_int_voidp(void* fn, void* a0) {\n\treturn ((int(*)(void*))fn)(a0);\n}",
		"static inline void cppgo_shim_void_voidp_int_int(void* fn, void* a0, int a1, int a2) {\n\t((void(*)(void*, int, int))fn)(a0, a1, a2);\n}",
		"static inline void* cppgo_shim_voidp_constcharp(void* fn, const char* a0) {\n\treturn ((void*(*)(const char*))fn)(a0);\n}",
	}
	for _, w := range want {
		if !strings.Contains(h, w) {
			t.Errorf("header missing dispatcher:\n%s\n--- full header ---\n%s", w, h)
		}
	}

	// A duplicate shape (another int(void*) getter) must NOT add a second dispatcher.
	registerShimShape(true, method{cFunc: "gen_QList_count", ret: retPlan{cRetType: "int"}}, "gen_qlist.h")
	if n := strings.Count(EmittedShimHeader(), "cppgo_shim_int_voidp("); n != 1 {
		t.Errorf("int(void*) dispatcher emitted %d times, want 1 (shapes must dedup)", n)
	}

	// Step-4 shape oracle: each routed shim gets a compile-time type-compat assertion; "no params"
	// is spelled (void); each referenced header is #included exactly once.
	vf := EmittedVerifyFile()
	wantV := []string{
		"#ifdef CPPGO_VERIFY",
		`_Static_assert(__builtin_types_compatible_p(__typeof__(gen_QString_size), int(void*)), "cppgo leg2 shape mismatch: gen_QString_size");`,
		`_Static_assert(__builtin_types_compatible_p(__typeof__(gen_QString_fromUtf8), void*(const char*)), "cppgo leg2 shape mismatch: gen_QString_fromUtf8");`,
		`#include "gen_qstring.h"`,
		`#include "gen_qwidget.h"`,
	}
	for _, w := range wantV {
		if !strings.Contains(vf, w) {
			t.Errorf("verify file missing:\n%s\n--- full ---\n%s", w, vf)
		}
	}
	if n := strings.Count(vf, `#include "gen_qstring.h"`); n != 1 {
		t.Errorf("gen_qstring.h #included %d times, want 1 (headers must dedup)", n)
	}
}
