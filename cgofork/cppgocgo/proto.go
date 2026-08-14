// cppgo splice — bypass loadDWARF's per-file gcc/DWARF probe for cppgo bindings.
//
// loadDWARF's job: given the C.xxx names a file uses + the preamble, return each name's C type
// as a dwarf.Type. Its COST is recompiling the full preamble under -g, once PER FILE.
//
// We skip that. Per guessKinds, every C.xxx name (except #define int/str consts) reaches
// loadDWARF, and for cppgo bindings every such name is one of a CLOSED, target-INDEPENDENT set:
//
//   (a) cppgo shims        -- flat prototypes in gen_*.h. Their types are, by construction of the
//                             emitter (gen.go:builtinMap), one of 12 scalars at pointer depth 0/1/2.
//                             Resolved by parsing the prototype + composing from harvested types.
//   (b) cgo builtins       -- builtinDefs: GoString/GoStringN/GoBytes/CString/CBytes/_CMalloc.
//   (c) basic C type names -- C.int/char/uchar/.../size_t. These ARE the scalars.
//   (d) libc the preamble  -- C.free etc. (cppgo emits the preamble; malloc/free/size_t covered).
//       declares
//
// We HARVEST the real dwarf.Types for the whole closed vocabulary ONCE (builtinProlog + <stdlib.h>
// + a scalar x ptr-depth grid + __typeof__ of each builtin) -- no per-file preamble, no hand-built
// dwarf structs. Then we compose shim signatures from prototypes. Any name we cannot resolve makes
// protoTypes return ok=false, so loadDWARF falls back to the real probe -> correct by construction.
package main

import (
	"bytes"
	"debug/dwarf"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// ---- prototype parsing (class a) ------------------------------------------------------------

var protoRe = regexp.MustCompile(`^\s*([\w ]+\**)\s+(\w+)\s*\(([^)]*)\)\s*;`)
var nameTail = regexp.MustCompile(`\s+\w+$`)

// shimRe matches a leg-2 shape dispatcher in cppgo_shims.h: `static inline RET cppgo_shim_x(void* fn, ARGS) {`.
// Unlike a gen_*.h prototype it has the `static inline` prefix and a `{` body, not a trailing `;`. The
// leading `void* fn` param IS part of the dispatcher's cgo signature, so it is kept like any other arg.
var shimRe = regexp.MustCompile(`^\s*static\s+inline\s+([\w ]+\**)\s+(cppgo_shim_\w+)\s*\(([^)]*)\)\s*\{`)

type protoSig struct {
	ret  string   // normalized C type, e.g. "char*", "void", "long long*"
	args []string // normalized C types
}

// normCtype canonicalizes a C type string. const IS preserved: real DWARF distinguishes
// `const char *` (*const char) from `char *` (*char), and the oracle-diff requires the match.
func normCtype(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, " *", "*")
	return strings.TrimSpace(s)
}

func parseHeaderInto(path string, cache map[string]*protoSig) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		m := protoRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		cache[m[2]] = parseProtoSig(normCtype(m[1]), strings.TrimSpace(m[3]))
	}
}

// parseShimsInto adds the leg-2 shape dispatchers (cppgo_shims.h) to the cache so leg 1's fast path
// recognizes the C.cppgo_shim_<shape> calls the generator now emits. Without this every dispatcher name
// misses the cache and the whole file falls back to the slow gcc discovery probes.
func parseShimsInto(path string, cache map[string]*protoSig) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		m := shimRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		cache[m[2]] = parseProtoSig(normCtype(m[1]), strings.TrimSpace(m[3]))
	}
}

// parseProtoSig builds a protoSig from a (normalized) return type and a raw C parameter list,
// stripping each parameter's trailing name. "void" / "" means no params.
func parseProtoSig(ret, argstr string) *protoSig {
	sig := &protoSig{ret: ret}
	if argstr != "" && argstr != "void" {
		for _, a := range strings.Split(argstr, ",") {
			a = strings.TrimSpace(a)
			if loc := nameTail.FindStringIndex(a); loc != nil { // strip trailing param name
				if cand := strings.TrimSpace(a[:loc[0]]); cand != "" {
					a = cand
				}
			}
			sig.args = append(sig.args, normCtype(a))
		}
	}
	return sig
}

var (
	protoCache map[string]*protoSig
	protoOnce  sync.Once
)

// loadProtos scans for gen_*.h once. cgo runs with cwd = the package source dir, so os.Getwd()
// is the primary location; also try *srcDir and the -I dirs.
func (p *Package) loadProtos() {
	protoOnce.Do(func() {
		protoCache = map[string]*protoSig{}
		dirs := map[string]bool{}
		if wd, err := os.Getwd(); err == nil {
			dirs[wd] = true
		}
		if *srcDir != "" {
			dirs[*srcDir] = true
		}
		for i, o := range p.GccOptions {
			if o == "-I" && i+1 < len(p.GccOptions) {
				dirs[p.GccOptions[i+1]] = true
			} else if strings.HasPrefix(o, "-I") {
				dirs[o[2:]] = true
			}
		}
		for d := range dirs {
			hs, _ := filepath.Glob(filepath.Join(d, "gen_*.h"))
			for _, h := range hs {
				parseHeaderInto(h, protoCache)
			}
			// leg 2: the shape dispatchers live in cppgo_shims.h, not gen_*.h — without these every
			// C.cppgo_shim_<shape> call would miss the cache and force the slow gcc discovery path.
			parseShimsInto(filepath.Join(d, "cppgo_shims.h"), protoCache)
		}
	})
}

// ---- one-shot harvest of the closed vocabulary (real dwarf.Types) ---------------------------

// the 12 scalars cppgo's emitter can produce (gen.go:builtinMap). void is handled as a nil type.
var harvestScalars = []string{
	"char", "unsigned char", "short", "unsigned short", "int", "unsigned int",
	"long", "long long", "unsigned long long", "float", "double",
}

// cgo builtins (builtinDefs) + libc the preamble may declare. Harvested via __typeof__.
var harvestFuncs = []string{
	"GoString", "GoStringN", "GoBytes", "CString", "CBytes", "_CMalloc", "malloc", "free",
}

var (
	harvestOnce sync.Once
	harvestType map[string]dwarf.Type // key: ctype string ("int","int*","void*",...) or func name
	voidType    dwarf.Type            // the real dwarf VoidType (for void returns), from void*'s pointee
	harvestErr  bool
)

func (p *Package) harvest() {
	harvestOnce.Do(func() {
		harvestType = map[string]dwarf.Type{}
		var b bytes.Buffer
		b.WriteString(builtinProlog)
		b.WriteString("#include <stdlib.h>\n")
		b.WriteString("#line 1 \"cppgo-harvest\"\n")

		var keys []string
		decl := func(typeExpr, key string) {
			fmt.Fprintf(&b, "%s *__cgo_h%d;\n", typeExpr, len(keys))
			keys = append(keys, key)
		}
		// scalar x pointer-depth grid: "int", "int*", "int**", plus void*/void** and size_t.
		for _, s := range append([]string{"size_t"}, harvestScalars...) {
			decl(s, s)
			decl(s+"*", s+"*")
			decl(s+"**", s+"**")
		}
		decl("void*", "void*")
		decl("void**", "void**")
		// the only const-qualified flat type the emitter produces (gen.go) is const char*.
		decl("const char*", "const char*")
		decl("const char**", "const char**")
		// type-only names that appear as C.xxx (kind=type): builtinProlog typedefs + <stddef.h>.
		for _, t := range []string{"ptrdiff_t", "intgo", "_GoString_", "_GoBytes_"} {
			decl(t, t)
		}
		// builtin / libc functions, real signatures via __typeof__.
		for _, fn := range harvestFuncs {
			fmt.Fprintf(&b, "__typeof__(%s) *__cgo_h%d;\n", fn, len(keys))
			keys = append(keys, fn)
		}

		d, _, _, _ := p.gccDebug(b.Bytes(), 0)
		r := d.Reader()
		for {
			e, err := r.Next()
			if err != nil || e == nil {
				break
			}
			if e.Tag == dwarf.TagVariable {
				name, _ := e.Val(dwarf.AttrName).(string)
				if idx, ok := strings.CutPrefix(name, "__cgo_h"); ok {
					if i, err := strconv.Atoi(idx); err == nil && i < len(keys) {
						if off, ok := e.Val(dwarf.AttrType).(dwarf.Offset); ok {
							if typ, err := d.Type(off); err == nil {
								if pt, ok := typ.(*dwarf.PtrType); ok {
									harvestType[keys[i]] = pt.Type // strip the synthetic pointer
								}
							}
						}
					}
				}
			}
			if e.Tag != dwarf.TagCompileUnit {
				r.SkipChildren()
			}
		}
		// the real void type is void*'s pointee — used for void RETURNS (real DWARF prints "void",
		// not a nil ReturnType).
		if pt, ok := harvestType["void*"].(*dwarf.PtrType); ok {
			voidType = pt.Type
		}
		// sanity: if the core scalars didn't harvest, disable the splice (fall back everywhere).
		for _, s := range []string{"int", "void*", "char*", "const char*"} {
			if _, ok := harvestType[s]; !ok {
				harvestErr = true
			}
		}
	})
}

// resolveCtype maps a normalized C type string to its harvested dwarf.Type.
// void -> (nil, true) for a valid void return; unknown -> (nil, false) to trigger fallback.
func resolveCtype(s string) (dwarf.Type, bool) {
	if s == "void" {
		return voidType, true
	}
	t, ok := harvestType[s]
	return t, ok
}

// ---- loadDefines bypass -----------------------------------------------------------------------

var (
	gccIsClangOnce sync.Once
	gccIsClangVal  bool
)

// loadDefinesFast determines gccIsClang ONCE (a compiler property) and SKIPS the per-file
// `gcc -dM` macro dump over the full preamble. Safe for cppgo: the oracle-verify proved no
// generated C.xxx name is a #define macro (every name has empty n.Define -> zero GK-MISS), so
// the macro-matching loop never sets anything. We only reproduce gccIsClang, which main.go itself
// already treats as a once-value (main.go: once.Do(p.GccIsClang = ...)).
func (f *File) loadDefinesFast(gccOptions []string) bool {
	gccIsClangOnce.Do(func() {
		out := gccDefines([]byte(builtinProlog), gccOptions) // tiny: no preamble
		gccIsClangVal = strings.Contains(out, "#define __clang__ ")
	})
	return gccIsClangVal
}

// ---- the bypass -----------------------------------------------------------------------------

// resolveName returns the dwarf.Type for one C.xxx name, or ok=false if outside the closed set.
func (p *Package) resolveName(n *Name) (dwarf.Type, bool) {
	if sig := protoCache[n.C]; sig != nil { // (a) shim: compose FuncType from the prototype
		rt, ok := resolveCtype(sig.ret)
		if !ok {
			return nil, false
		}
		ft := &dwarf.FuncType{CommonType: dwarf.CommonType{Name: "func"}, ReturnType: rt}
		for _, a := range sig.args {
			at, ok := resolveCtype(a)
			if !ok {
				return nil, false
			}
			ft.ParamType = append(ft.ParamType, at)
		}
		return ft, true
	}
	if n.Kind == "type" { // (c) basic C type / typedef name — n.C is the full C spelling
		return resolveCtype(normCtype(n.C))
	}
	if t := harvestType[n.C]; t != nil { // (b)/(d) builtin or libc function
		return t, true
	}
	return nil, false // unforeseen -> fall back
}

var (
	missMu   sync.Mutex
	missSeen = map[string]bool{}
)

func logMiss(n *Name) {
	if os.Getenv("CPPGO_MISS") == "" {
		return
	}
	missMu.Lock()
	defer missMu.Unlock()
	k := n.Kind + " " + n.C
	if !missSeen[k] {
		missSeen[k] = true
		fmt.Fprintf(os.Stderr, "SPLICE-MISS kind=%s name=%s\n", n.Kind, n.C)
	}
}

// isBuiltinFunc reports whether n.C is one of the cgo builtins / libc funcs we harvest.
var builtinFuncSet = func() map[string]bool {
	m := map[string]bool{}
	for _, f := range harvestFuncs {
		m[f] = true
	}
	return m
}()

// classifyKind returns the Kind guessKinds' gcc-sniff would assign, WITHOUT a probe, or ok=false
// if the name is outside our closed set. Mirrors guessKinds: a function (shim or builtin) sniffs as
// "not-type"; a C type name sniffs as "type". (#define consts are excluded -> handled by fallback.)
func classifyKind(n *Name) (string, bool) {
	if n.Define != "" {
		return "", false // let real guessKinds handle #define consts
	}
	if protoCache[n.C] != nil || builtinFuncSet[n.C] {
		return "not-type", true
	}
	if _, ok := resolveCtype(normCtype(n.C)); ok {
		return "type", true
	}
	return "", false
}

// guessKindsFast sets every name's Kind from the closed-set tables and returns needType (all names),
// skipping guessKinds' per-file gcc sniff. ok=false (no mutation done) -> caller runs real guessKinds.
func (p *Package) guessKindsFast(f *File) ([]*Name, bool) {
	p.loadProtos()
	p.harvest()
	if harvestErr {
		return nil, false
	}
	kinds := make(map[*Name]string, len(f.Name))
	for _, key := range nameKeys(f.Name) {
		n := f.Name[key]
		k, ok := classifyKind(n)
		if !ok {
			return nil, false
		}
		kinds[n] = k
	}
	needType := make([]*Name, 0, len(kinds))
	for n, k := range kinds {
		n.Kind = k
		needType = append(needType, n)
	}
	return needType, true
}

func (p *Package) protoTypes(names []*Name) ([]dwarf.Type, bool) {
	p.loadProtos()
	p.harvest()
	if harvestErr {
		return nil, false
	}
	types := make([]dwarf.Type, len(names))
	ok := true
	for i, n := range names {
		t, good := p.resolveName(n)
		if !good {
			logMiss(n)
			ok = false
			continue
		}
		types[i] = t
	}
	return types, ok // false if ANY name unresolved -> loadDWARF falls back for this file
}
