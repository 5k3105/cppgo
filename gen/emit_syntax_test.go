package gen

// Emit-and-parse harness: run EmitClass / EmitFunctions / the support emitters over a synthetic
// 3-level hierarchy exercising every marshalling family (ctor, scalar/string/handle/out-param/float
// params, record returns, signals, virtuals incl. QString-returning, name-hiding re-declarations,
// the base-chain bind) for BOTH backends, and feed every emitted Go file through go/parser. This is
// the cheap regression net for generator changes: emitted-Go syntax errors surface here, in
// milliseconds, instead of at the next full Qt regen.

import (
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/5k3105/cppgo/ir"
)

func synModule() ([]ir.Record, Resolver) {
	intT := ir.Type{Kind: "builtin", Name: "int"}
	voidT := ir.Type{Kind: "builtin", Name: "void"}
	doubleT := ir.Type{Kind: "builtin", Name: "double"}
	qstringT := ir.Type{Kind: "record", Name: "QString"}
	boolT := ir.Type{Kind: "builtin", Name: "bool"}
	rootPtrT := ir.Type{Kind: "pointer", Elem: &ir.Type{Kind: "record", USR: "u:root", Name: "TRoot"}}

	value := ir.Method{Name: "value", Ret: intT}
	root := ir.Record{
		USR: "u:root", Name: "TRoot", Header: "troot.h",
		DefaultCtor: true, PublicDtor: true, Copyable: true,
		Ctors: []ir.Method{{Name: "TRoot", Ret: voidT}},
		Methods: []ir.Method{
			value,
			{Name: "setTitle", Ret: voidT, Params: []ir.Param{{Name: "title", Type: qstringT}}},
			{Name: "attach", Ret: voidT, Params: []ir.Param{{Name: "other", Type: rootPtrT}}},
			{Name: "probe", Ret: intT, Params: []ir.Param{{Name: "ok", Type: ir.Type{Kind: "pointer", Elem: &boolT}}}},
			{Name: "scale", Ret: doubleT, Params: []ir.Param{{Name: "f", Type: doubleT}}},
			{Name: "total", Static: true, Ret: intT},
			{Name: "changed", Signal: true, Ret: voidT, Params: []ir.Param{{Name: "n", Type: intT}}},
			{Name: "onTick", Virtual: true, Own: true, Ret: voidT, Params: []ir.Param{{Name: "dt", Type: intT}}},
			{Name: "label", Virtual: true, Own: true, Ret: qstringT},
		},
	}
	mid := ir.Record{
		USR: "u:mid", Name: "TMid", Header: "tmid.h",
		DefaultCtor: true, PublicDtor: true, Copyable: true,
		Bases: []ir.Base{{USR: "u:root", Name: "TRoot", Access: "public"}},
		Ctors: []ir.Method{{Name: "TMid", Ret: voidT}},
		Methods: []ir.Method{
			{Name: "midOnly", Own: true, Ret: intT},
			value, // inherited-flattened copy (Own=false) -> de-flattens onto embedding
		},
	}
	der := ir.Record{
		USR: "u:der", Name: "TDerived", Header: "tderived.h",
		DefaultCtor: true, PublicDtor: true, Copyable: true,
		Bases: []ir.Base{{USR: "u:mid", Name: "TMid", Access: "public"}},
		Ctors: []ir.Method{{Name: "TDerived", Ret: voidT}},
		Methods: []ir.Method{
			// C++ name hiding: an OWN non-virtual re-declaration with a base-matching signature must be
			// RE-EMITTED (the base shim would statically call the base impl).
			{Name: "value", Own: true, Ret: intT},
		},
	}
	recs := []ir.Record{root, mid, der}
	rv := emptyRV("qt")
	rv.Records = map[string]*ir.Record{}
	for i := range recs {
		r := &recs[i]
		rv.Bound[r.USR] = true
		rv.Name[r.USR] = r.Name
		rv.Copyable[r.USR] = true
		rv.Header[r.USR] = r.Header
		rv.Pkg[r.USR] = "qt"
		rv.Records[r.USR] = r
	}
	rv.MethodKeys = BuildMethodKeys(recs)
	return recs, rv
}

func mustParse(t *testing.T, label, src string) {
	t.Helper()
	if _, err := parser.ParseFile(token.NewFileSet(), label+".go", src, 0); err != nil {
		lines := strings.Split(src, "\n")
		var num []string
		for i, l := range lines {
			num = append(num, strings.Repeat(" ", 4-len(itoa(i+1)))+itoa(i+1)+" "+l)
		}
		t.Fatalf("%s: emitted Go does not parse: %v\n%s", label, err, strings.Join(num, "\n"))
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func testEmitBackend(t *testing.T, backend string) {
	shimShapes = map[string]shimShape{}
	cbShapes = map[string]cbShape{}
	shimVerify, shimVerifyHdrs, shimVerifySeen = nil, nil, map[string]bool{}

	recs, rv := synModule()
	rv.Backend = backend
	for i := range recs {
		r := recs[i]
		goSrc, hSrc, cppSrc, _, renderable := EmitClass(r, "qt", rv)
		if !renderable {
			t.Fatalf("%s: %s unexpectedly non-renderable", backend, r.Name)
		}
		if hSrc == "" || cppSrc == "" {
			t.Fatalf("%s: %s emitted empty header/shim", backend, r.Name)
		}
		mustParse(t, backend+"_"+r.Name, goSrc)
		// The derived class must use the one-crossing chain bind (TDerived -> TMid -> TRoot).
		if r.Name == "TDerived" {
			if !strings.Contains(goSrc, "TDerived_chain") || !strings.Contains(cppSrc, "TDerived_chain") {
				t.Errorf("%s: TDerived bind does not use the chain shim", backend)
			}
			// Name hiding: the own re-declared value() must be re-emitted, not de-flattened.
			if !strings.Contains(goSrc, ") Value(") {
				t.Errorf("%s: TDerived lost its re-declared value() (name hiding regressed)", backend)
			}
		}
		if r.Name == "TMid" && strings.Count(goSrc, ") Value(") != 0 {
			t.Errorf("%s: TMid re-emitted the inherited value() (de-flatten regressed)", backend)
		}
		if r.Name == "TRoot" {
			// The virtual crossing must be gated on the per-instance override mask.
			if !strings.Contains(cppSrc, "__goOn[") {
				t.Errorf("%s: TRoot virtual overrides are not mask-gated", backend)
			}
			// Delete must release the whole wrapper chain and drop registry entries.
			for _, want := range []string{"UnsafeRelease()", "unregisterInstance"} {
				if !strings.Contains(goSrc, want) {
					t.Errorf("%s: TRoot Delete/relase missing %s", backend, want)
				}
			}
		}
	}
	// Free functions: one with a QString param (string marshalling) + one float (purego fallback).
	fns := []ir.Func{
		{Name: "tglobal", CppName: "tglobal", Ret: ir.Type{Kind: "builtin", Name: "int"},
			Params: []ir.Param{{Name: "s", Type: ir.Type{Kind: "record", Name: "QString"}}}},
		{Name: "tscale", CppName: "tscale", Ret: ir.Type{Kind: "builtin", Name: "double"},
			Params: []ir.Param{{Name: "f", Type: ir.Type{Kind: "builtin", Name: "double"}}}},
	}
	if fg, fh, fc := EmitFunctions(fns, "qt", rv); fg != "" {
		mustParse(t, backend+"_functions", fg)
		if fh == "" || fc == "" {
			t.Fatalf("%s: free functions emitted empty header/shim", backend)
		}
	} else {
		t.Fatalf("%s: free functions emitted nothing", backend)
	}
	if backend == "purego" {
		mustParse(t, "purego_support", PuregoSupport("qt"))
		u, w := PuregoSupportOS("qt")
		mustParse(t, "purego_support_unix", u)
		mustParse(t, "purego_support_windows", w)
		if cb := EmittedCbGo("qt"); cb != "" {
			mustParse(t, "purego_cb", cb)
		} else {
			t.Fatalf("purego: no callback dispatch emitted despite signals+virtuals")
		}
	} else {
		if EmittedShimHeader() == "" {
			t.Fatalf("cgo: no shim shapes registered")
		}
	}
}

func TestEmittedGoParsesCgo(t *testing.T)    { testEmitBackend(t, "") }
func TestEmittedGoParsesPurego(t *testing.T) { testEmitBackend(t, "purego") }

// TestDataModelWidths pins the width-ambiguous C types across the two target data models —
// the whole substance of the Windows (LLP64) port at the type level.
func TestDataModelWidths(t *testing.T) {
	defer SetDataModel("lp64") // global — restore for other tests
	cases := []struct {
		model, name, wantC, wantGo, wantReg string
	}{
		{"lp64", "long", "long", "int", "int64"},
		{"lp64", "unsigned long", "unsigned long", "uint", "int64"},
		{"lp64", "wchar_t", "int", "int32", "int32"},
		{"llp64", "long", "long", "int32", "int32"},
		{"llp64", "unsigned long", "unsigned long", "uint32", "int32"},
		{"llp64", "wchar_t", "unsigned short", "uint16", "int16"},
		// invariant across both models:
		{"lp64", "long long", "long long", "int64", "int64"},
		{"llp64", "long long", "long long", "int64", "int64"},
		{"llp64", "int", "int", "int", "int32"},
	}
	for _, c := range cases {
		SetDataModel(c.model)
		gotC, gotGo, ok := builtinMap(c.name)
		if !ok || gotC != c.wantC || gotGo != c.wantGo {
			t.Errorf("%s %q: builtinMap = (%q, %q, %v), want (%q, %q, true)",
				c.model, c.name, gotC, gotGo, ok, c.wantC, c.wantGo)
		}
		if gotReg := pgRegType(gotC); gotReg != c.wantReg {
			t.Errorf("%s %q: pgRegType(%q) = %q, want %q", c.model, c.name, gotC, gotReg, c.wantReg)
		}
	}
	// The setter must reject junk loudly, not mis-generate widths.
	func() {
		defer func() {
			if recover() == nil {
				t.Errorf("SetDataModel accepted an unknown model")
			}
		}()
		SetDataModel("ilp32")
	}()
}
