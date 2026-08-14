package gen

import (
	"regexp"
	"testing"

	"github.com/5k3105/cppgo/ir"
)

// emptyRV builds a minimal Resolver for emitter-only tests (no extraction needed).
func emptyRV(pkg string) Resolver {
	return Resolver{
		Bound: map[string]bool{}, Name: map[string]string{}, Copyable: map[string]bool{},
		Header: map[string]string{}, EnumName: map[string]string{}, EnumHeader: map[string]string{},
		EnumPublic: map[string]bool{}, Pkg: map[string]string{}, EnumPkg: map[string]string{},
		MethodKeys: map[string]map[string]bool{}, FreeOps: map[string][]ir.Func{},
		Ancestors: map[string]map[string]bool{}, Self: pkg,
	}
}

var cppgofnSym = regexp.MustCompile(`\bcppgofn_\w+`)

func emittedFreeSymbols(cppSrc string) map[string]bool {
	out := map[string]bool{}
	for _, s := range cppgofnSym.FindAllString(cppSrc, -1) {
		out[s] = true
	}
	return out
}

// TestFreeFunctionSymbolsArePackageUnique is the GENERATOR-LEVEL guard for the cross-package
// "multiple definition" bug class — it tests the emitter directly, with NO extraction, gen, or link.
// Invariant: every exported C symbol must be globally unique across any two packages that can be linked
// into one executable. Free functions are the only package-global symbols (class shims are unique via the
// class name), so a free function of the SAME name in two packages must emit DIFFERENT C symbols. Before
// the package-qualified fix this FAILED (both -> cppgofn_reset); it now passes (cppgofn_qt_reset vs
// cppgofn_kf6_reset). The post-gen scanner tools/check_symbol_collisions.py is the second line; this is the
// first — it would have caught the bug before any regen ever ran.
func TestFreeFunctionSymbolsArePackageUnique(t *testing.T) {
	fns := []ir.Func{{Name: "reset", CppName: "reset", Ret: ir.Type{Kind: "builtin", Name: "void"}}}

	_, _, cppQt := EmitFunctions(fns, "qt", emptyRV("qt"))
	_, _, cppKf6 := EmitFunctions(fns, "kf6", emptyRV("kf6"))

	a, b := emittedFreeSymbols(cppQt), emittedFreeSymbols(cppKf6)
	if len(a) == 0 || len(b) == 0 {
		t.Fatalf("no cppgofn_ symbols emitted (qt=%d kf6=%d); test setup broken", len(a), len(b))
	}
	for s := range a {
		if b[s] {
			t.Errorf("free-function C symbol %q is emitted by BOTH packages -> multiple-definition at the "+
				"executable link. Free-function symbols must be package-qualified (cppgofn_<pkg>_<name>).", s)
		}
	}
}
