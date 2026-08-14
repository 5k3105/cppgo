// gen emits binding files (Go + C-ABI .h + C++ .cpp) for one or more classes into a
// buildable package.
//
// Usage: gen <outdir> <Class1,Class2,...> <ir.json>...
package main

import (
	"crypto/sha256"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/5k3105/cppgo/closure"
	"github.com/5k3105/cppgo/gen"
	"github.com/5k3105/cppgo/ir"
)

// unlinkableList is the curated "in the header but not in the library" residual — classes that compile but
// are an undefined reference at the executable link. DERIVED FROM THE LINK (tools/derive_unlinkable.sh), not
// the header, and embedded at gen-build time. See cmd/gen/unlinkable.txt.
//
//go:embed unlinkable.txt
var unlinkableList string

func loadUnlinkable() map[string]bool {
	m := map[string]bool{}
	for _, line := range strings.Split(unlinkableList, "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		if line = strings.TrimSpace(line); line != "" {
			m[line] = true
		}
	}
	return m
}

// Build-speed flags on the shim C++ compilation:
//
//	CFLAGS -O0 -g0   for cgo's OWN generated C — the ~13.6k `_cgo` trampolines (.cgo2.c) + _cgo_gotypes.c.
//	                 cgo compiles them at its default -O2 -g; they're trivial calling-convention forwarders
//	                 that need neither optimization nor DWARF. We previously set only CXXFLAGS (our .cpp),
//	                 so the C glue silently kept -O2 -g — the bulk of the per-package .cgo2.c compile.
//	                 Last-flag-wins, so -O0 -g0 appended after cgo's -O2 -g takes effect. See
//
// — measured ~14% off the Go-side compile, free.
//
//	CXXFLAGS -O0 -g0 the shims are one-line forwarders to Qt's already-compiled libs; optimization +
//	                 debug info are pure waste (overrides go's cgo default of -g -O2; last flag wins).
//	-include cppgo_pch.h  force-include the common Qt header set so a precompiled header (cppgo_pch.h.gch,
//	                 built once by the build script) is reused across every shim TU instead of re-parsing
//	                 those template-heavy headers ~685 times. Degrades gracefully: if no/mismatched .gch,
//	                 GCC just parses the header (correct, no speedup).
//
// BINARY SIZE (miqt issue #147): cross-language link-time DCE — -ffunction-sections
// -fdata-sections per shim + -Wl,--gc-sections to drop every C-ABI shim the app never calls — is OPT-IN
// at APP-BUILD time, NOT baked into these #cgo directives. Go's cgo flag whitelist REJECTS
// -ffunction-sections in #cgo CFLAGS/CXXFLAGS (`invalid flag in #cgo CFLAGS`), so baking it in would
// break every `go build` unless each consumer sets CGO_*_ALLOW. Enable it per-app instead, e.g.:
//
//	CGO_CXXFLAGS_ALLOW='-ffunction-sections|-fdata-sections' \
//	CGO_CXXFLAGS='-ffunction-sections -fdata-sections' \
//	go build -ldflags '-s -w -extldflags=-Wl,--gc-sections'
//
// The proven size recipe acting on the final app binary is `go build -ldflags "-s -w"` (+ optional upx) —
// see BUILDING.md. (cppgo binds the WHOLE Qt surface, so an app uses a fraction; size control is an
// app-build concern.)
const supportGo = `package genqt

/*
#cgo CFLAGS: -O0 -g0
#cgo CXXFLAGS: -std=c++17 -O0 -g0 -include cppgo_pch.h CPPGO_CXX
#cgo LDFLAGS: ${SRCDIR}/cshim/libshim.a -lstdc++ -lm CPPGO_LIBS
#include <stdlib.h>
*/
import "C"

// b2i bridges Go bool <-> the C ABI's int (0/1) representation of C++ bool.
func b2i(b bool) C.int {
	if b {
		return 1
	}
	return 0
}
`

// pchHeader is the common Qt/STL header set every shim needs, factored into ONE header so the build can
// precompile it (cppgo_pch.h.gch) and `-include` it everywhere — the per-shim re-parse of these
// template-heavy headers was the dominant C++ compile cost. emitCpp no longer emits these per shim.
const pchHeader = `#pragma once
#include <QtCore/qlist.h>
#include <QtCore/qset.h>
#include <QtCore/qmap.h>
#include <QtCore/qhash.h>
#include <QtCore/qstring.h>
#include <QtCore/qbytearray.h>
#include <QtCore/qstringview.h>
#include <QtCore/qanystringview.h>
#include <QtCore/qobject.h>
#include <utility>
#include <cstdlib>
#include <cstring>
#include <unordered_map>
#include <mutex>
`

// callbackGo is the per-package registry that lets a Go signal-callback be referenced from C by an
// integer id (a Go func can't be passed to C). The //export trampolines look the func up by id.
// Slice-indexed registries (ids are dense, allocated here) + RWMutex: deliveries only read, and the
// virtual path does a tiny linear scan over the instance's registered slots — no per-delivery
// hashing or allocation. unregister* releases closures (Disconnect / Delete), fixing the old
// registry leak in connect/disconnect-heavy long-running apps.
const callbackGo = `package genqt

import (
	"sync"
	"unsafe"
)

var (
	cbMu  sync.RWMutex
	cbReg []any
)

func registerCallback(fn any) int64 {
	cbMu.Lock()
	defer cbMu.Unlock()
	cbReg = append(cbReg, fn)
	return int64(len(cbReg))
}

func lookupCallback(id int64) any {
	cbMu.RLock()
	defer cbMu.RUnlock()
	if id < 1 || int(id) > len(cbReg) {
		return nil
	}
	return cbReg[id-1]
}

func unregisterCallback(id int64) {
	cbMu.Lock()
	defer cbMu.Unlock()
	if id >= 1 && int(id) <= len(cbReg) {
		cbReg[id-1] = nil
	}
}

// Virtual-override registry: per __Go instance (goid) -> (slot id -> Go impl). Slot ids are
// generator-computed FNV hashes of the virtual's slot key — identical across classes and packages,
// so a registration through a promoted base wrapper still matches the derived subclass's dispatch.
type virtEnt struct {
	slot int64
	fn   any
}

// enableEnt flips one bit of the instance's C++-side override mask (__goOn) at registration time,
// so an UNREGISTERED virtual never crosses into Go — the C++ override falls straight to the base.
type enableEnt struct {
	h      unsafe.Pointer
	idxOf  func(slot int64) int32
	enable func(h unsafe.Pointer, idx int32)
}

var (
	vMu     sync.RWMutex
	vReg    [][]virtEnt // index = goid-1; ids are never reused (an empty slot per dead instance, not a leak class)
	vEnable []enableEnt // index = goid-1
)

func nextVirtID() int64 {
	vMu.Lock()
	defer vMu.Unlock()
	vReg = append(vReg, nil)
	vEnable = append(vEnable, enableEnt{})
	return int64(len(vReg))
}

func registerInstance(id int64, h unsafe.Pointer, idxOf func(int64) int32, enable func(unsafe.Pointer, int32)) {
	vMu.Lock()
	defer vMu.Unlock()
	if id >= 1 && int(id) <= len(vEnable) {
		vEnable[id-1] = enableEnt{h: h, idxOf: idxOf, enable: enable}
	}
}

func registerVirt(id int64, slot int64, fn any) {
	if id < 1 { // not a Go-constructed instance (borrowed wrapper): nothing to override
		return
	}
	vMu.Lock()
	if int(id) > len(vReg) {
		vMu.Unlock()
		return
	}
	ents := vReg[id-1]
	replaced := false
	for i := range ents {
		if ents[i].slot == slot {
			ents[i].fn = fn
			replaced = true
			break
		}
	}
	if !replaced {
		vReg[id-1] = append(ents, virtEnt{slot: slot, fn: fn})
	}
	e := vEnable[id-1]
	vMu.Unlock()
	if e.enable != nil && e.idxOf != nil {
		if idx := e.idxOf(slot); idx >= 0 {
			e.enable(e.h, idx) // flip the C++ mask bit: this virtual now crosses into Go
		}
	}
}

func lookupVirt(id int64, slot int64) any {
	vMu.RLock()
	defer vMu.RUnlock()
	if id < 1 || int(id) > len(vReg) {
		return nil
	}
	for _, e := range vReg[id-1] {
		if e.slot == slot {
			return e.fn
		}
	}
	return nil
}

func unregisterInstance(id int64) {
	vMu.Lock()
	defer vMu.Unlock()
	if id >= 1 && int(id) <= len(vReg) {
		vReg[id-1] = nil
		vEnable[id-1] = enableEnt{}
	}
}
`

// pkgOf maps a defining header to its owning Go package. QtCore/QtGui/QtWidgets (and anything else)
// form the base "qt" package; the higher tiers each get their own package that imports the base.
func pkgOf(h string) string {
	switch {
	case strings.Contains(h, "/QtWebEngine"):
		return "webengine"
	case strings.Contains(h, "/QtPdf"):
		return "pdf"
	case strings.Contains(h, "/KF6/"):
		return "kf6"
	default:
		return "qt"
	}
}

func main() {
	if len(os.Args) < 6 {
		fmt.Fprintln(os.Stderr, "usage: gen <outdir> <pkg> <baseimport|-> <all|Class1,...> <ir.json>...")
		os.Exit(2)
	}
	outdir, pkg, baseImport, classArg := os.Args[1], os.Args[2], os.Args[3], os.Args[4]
	if baseImport == "-" {
		baseImport = ""
	}
	// Target C data model: lp64 (default; Linux/macOS x86-64) or llp64 (Windows). Affects the
	// spelled-`long`/`wchar_t` width mapping only — pair it with IR extracted from the TARGET
	// platform's headers (clang resolves size_t & friends per target).
	if m := os.Getenv("CPPGO_DATAMODEL"); m != "" {
		gen.SetDataModel(m)
		fmt.Fprintf(os.Stderr, "data model: %s\n", m)
	}
	m, err := ir.Load(os.Args[5:]...)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load:", err)
		os.Exit(1)
	}

	// Resolver knows every bound record's USR -> name/package (so cross-package refs resolve and
	// qualify) and every enum's USR -> name/package. Pkg/EnumPkg cover ALL records, not just this
	// package's, so a `pkg` shim referencing a base type emits `qt.QWidget` + the import.
	rv := gen.Resolver{
		Bound: closure.BoundSet(m), Name: map[string]string{}, Copyable: map[string]bool{},
		Header: map[string]string{}, EnumName: map[string]string{}, EnumHeader: map[string]string{},
		EnumPublic: map[string]bool{}, Enum64: map[string]bool{},
		Pkg: map[string]string{}, EnumPkg: map[string]string{}, Self: pkg, BaseImport: baseImport,
		Backend: os.Getenv("CPPGO_BACKEND"), // "" / "cgo" (default) or "purego"
		Records: m.RecordUSR,                // enables the one-crossing base-chain bind
	}
	// Curated IRREDUCIBLE LINK-ONLY residual: classes that COMPILE but have no exported symbol in the
	// library (their ctor/operator isn't *_EXPORT'd) -> binding them is an undefined reference when an
	// EXECUTABLE links (not when the package archive builds). Found by a link pass; kept small. Dropping
	// from Bound un-emits the class AND makes any method referencing it planFail+drop. Gen-side (not the
	// frontend) so adding one needs only a re-gen, no re-extraction.
	unlinkable := loadUnlinkable()
	for _, r := range m.Records {
		rv.Name[r.USR] = r.Name
		rv.Copyable[r.USR] = r.Copyable
		rv.Header[r.USR] = r.Header
		rv.Pkg[r.USR] = pkgOf(r.Header)
		if unlinkable[r.Name] {
			delete(rv.Bound, r.USR)
		}
	}
	for _, e := range m.Enums {
		rv.EnumName[e.USR] = e.Name
		rv.EnumHeader[e.USR] = e.Header
		rv.EnumPkg[e.USR] = pkgOf(e.Header)
		rv.EnumPublic[e.USR] = e.Public
		// An enum with enumerators outside 32 bits (QAnyStringView::Tag) would TRUNCATE through the
		// default C `int` ABI crossing — flag it so plan crosses it as long long instead.
		for _, c := range e.Consts {
			if v, err := strconv.ParseInt(c.Value, 10, 64); err == nil {
				if v > 0xFFFFFFFF || v < -0x80000000 {
					rv.Enum64[e.USR] = true
				}
			} else if u, err2 := strconv.ParseUint(c.Value, 10, 64); err2 == nil && u > 0xFFFFFFFF {
				rv.Enum64[e.USR] = true
			}
		}
	}
	// Per-record flattened-method signature sets, so a derived class can de-flatten (skip re-emitting)
	// any method already provided by its embedded primary base. Covers ALL loaded records (incl. base-
	// package records present in the IR) so cross-package inheritance de-flattens too.
	rv.MethodKeys = gen.BuildMethodKeys(m.Records)

	// Transitive base-USR set per record (for the covariant-record-return check in EmitClass).
	directBases := map[string][]string{}
	for _, r := range m.Records {
		for _, b := range r.Bases {
			if b.USR != "" {
				directBases[r.USR] = append(directBases[r.USR], b.USR)
			}
		}
	}
	rv.Ancestors = map[string]map[string]bool{}
	var walk func(usr string, into map[string]bool)
	walk = func(usr string, into map[string]bool) {
		for _, b := range directBases[usr] {
			if !into[b] {
				into[b] = true
				walk(b, into)
			}
		}
	}
	for _, r := range m.Records {
		set := map[string]bool{}
		walk(r.USR, set)
		rv.Ancestors[r.USR] = set
	}

	// Free/friend operators -> attach to their FIRST record argument (bound as methods in EmitClass).
	// Only when the operator and that record live in the SAME package (else the record's shim can't see
	// the operator's declaring header). Skip pointer-lhs operators.
	rv.FreeOps = map[string][]ir.Func{}
	for _, f := range m.Functions {
		if !strings.HasPrefix(f.Name, "operator") || len(f.Params) == 0 {
			continue
		}
		t := f.Params[0].Type
		usr := ""
		switch {
		case t.Kind == "record":
			usr = t.USR
		case t.Kind == "ref" && t.Elem != nil && t.Elem.Kind == "record":
			usr = t.Elem.USR
		}
		if usr == "" || !rv.Bound[usr] || rv.Pkg[usr] != pkgOf(f.Header) {
			continue
		}
		rv.FreeOps[usr] = append(rv.FreeOps[usr], f)
	}

	// Closure fixpoint: drop bound records that render NO members (opaque, all-boilerplate handles like
	// QWidgetData/QInternal) AND are referenced by nothing bindable, repeating until stable. This is the
	// pruning step closure.BoundSet can't do alone (renderability needs the emitter; closure may not
	// import gen). rv.Bound is shared with this whole run, so the selection below sees the pruned set.
	if pruned := gen.Fixpoint(m, &rv); len(pruned) > 0 {
		fmt.Fprintf(os.Stderr, "fixpoint: pruned %d zero-member unreferenced records: %s\n",
			len(pruned), strings.Join(pruned, ", "))
	}

	if err := os.MkdirAll(outdir, 0755); err != nil {
		panic(err)
	}

	// Select records to emit for THIS package: "all" = every bound record whose owning package is
	// `pkg`; otherwise the named subset (still restricted to this package). rv.Bound stays the FULL
	// bound set so cross-package references resolve and qualify (the referenced type is emitted in its
	// own package). The build script runs gen once per package and links them in one module.
	var recs []*ir.Record
	if classArg == "all" {
		for i := range m.Records {
			r := &m.Records[i]
			if rv.Bound[r.USR] && pkgOf(r.Header) == pkg {
				recs = append(recs, r)
			}
		}
	} else {
		for _, cls := range strings.Split(classArg, ",") {
			for i := range m.Records {
				if m.Records[i].Name == cls && pkgOf(m.Records[i].Header) == pkg {
					recs = append(recs, &m.Records[i])
					break
				}
			}
		}
	}

	// C++ TU grouping (per-class .go + .h always stay per-class; Go layout unchanged). CPPGO_UNITS sets
	// the number of aggregate C++ TUs: N>0 (default 24) = unity/jumbo — fewer header re-parses, best for a
	// COLD full build; 0 = per-class .cpp — finest ccache granularity (a 1-class change recompiles 1 TU),
	// best for INCREMENTAL dev. _cgo_export.h (no include guard) is prepended exactly once per .cpp that
	// holds any class with //export trampolines. (Switching modes needs a clean outdir — all.sh rm -rf's.)
	unitCount := 24
	if v := os.Getenv("CPPGO_UNITS"); v != "" {
		if u, err := strconv.Atoi(v); err == nil {
			unitCount = u
		}
	}
	type unit struct {
		body    strings.Builder
		exports bool
	}
	var units []unit
	if unitCount > 0 {
		units = make([]unit, unitCount)
	}
	// Leg 3: shim .cpp live OUTSIDE the Go package, in cshim/, so `go build`/cgo does NOT compile them —
	// the toolexec wrapper does, with the clang driver (clang++ -c, batched, full-header PCH), into
	// cshim/libshim.a, linked via #cgo LDFLAGS (gen_support.go). Headers (gen_*.h) stay in the package
	// (the .go preambles include them); the driver gets -I<pkg>.
	cshimDir := filepath.Join(outdir, "cshim")
	if err := os.MkdirAll(cshimDir, 0755); err != nil {
		panic(err)
	}
	seen := map[string]bool{}
	shimHash := sha256.New() // content stamp over every emitted shim body (see gen_shimstamp.go below)
	n := 0
	for _, rec := range recs {
		base := strings.ToLower(strings.ReplaceAll(rec.Name, "::", "__"))
		if seen[base] {
			continue
		}
		seen[base] = true
		goSrc, hSrc, cppSrc, hasExports, _ := gen.EmitClass(*rec, pkg, rv)
		shimHash.Write([]byte(cppSrc))
		write(filepath.Join(outdir, "gen_"+base+".go"), goSrc)
		write(filepath.Join(outdir, "gen_"+base+".h"), hSrc)
		// _cgo_export.h is no longer #included in the source: the toolexec wrapper supplies cgo's own copy
		// via -include from the build's -objdir, so the cshim .cpp stay package-location-independent.
		_ = hasExports
		if unitCount <= 0 { // per-class .cpp (fine-grained ccache)
			write(filepath.Join(cshimDir, "gen_"+base+".cpp"), cppSrc)
		} else { // accumulate into an aggregate unit
			u := &units[n%unitCount]
			u.body.WriteString(cppSrc)
			u.body.WriteString("\n")
		}
		n++
	}
	for i := range units {
		if units[i].body.Len() == 0 {
			continue
		}
		write(filepath.Join(cshimDir, fmt.Sprintf("gen_unit%d.cpp", i)), units[i].body.String())
	}
	mode := fmt.Sprintf("%d C++ units", unitCount)
	if unitCount <= 0 {
		mode = "per-class C++"
	}
	fmt.Printf("package %s: emitted %d record(s) (%s) to %s\n", pkg, n, mode, outdir)

	// Free (non-operator) functions owned by this package -> package-level Go funcs (gen_functions.*).
	// Both backends: EmitFunctions emits a backend-appropriate Go file (purego binds via RegisterLibFunc)
	// plus the shared C-ABI header + C++ shim.
	var pkgFuncs []ir.Func
	for _, f := range m.Functions {
		if pkgOf(f.Header) == pkg {
			pkgFuncs = append(pkgFuncs, f)
		}
	}
	if fg, fh, fc := gen.EmitFunctions(pkgFuncs, pkg, rv); fg != "" {
		write(filepath.Join(outdir, "gen_functions.go"), fg)
		write(filepath.Join(outdir, "gen_functions.h"), fh)
		write(filepath.Join(cshimDir, "gen_functions.cpp"), fc)
		shimHash.Write([]byte(fc))
		fmt.Printf("package %s: emitted free functions\n", pkg)
	}
	if rv.Backend != "purego" {
		// cgo backend: cshim/libshim.a is built by the toolexec wrapper DURING the package's cgo step —
		// but the Go build cache keys only on .go inputs, so a shim-body-only change would cache-hit the
		// package, skip cgo, and never rebuild the archive (stale or missing libshim.a). Stamping the
		// shim-content hash into a .go constant makes any shim change a guaranteed cache miss. (The
		// purego backend builds its .so explicitly via buildso.sh — no stamp needed.)
		write(filepath.Join(outdir, "gen_shimstamp.go"), fmt.Sprintf(
			"package %s\n\n// Code generated by cppgo; DO NOT EDIT.\n// Content stamp over every cshim body: forces the Go build cache to re-run cgo\n// (and thus the toolexec shim build) whenever any shim changes.\nconst cppgoShimStamp = %q\n",
			pkg, fmt.Sprintf("%x", shimHash.Sum(nil))))
	}
	// Typed Go enum aliases (so an enum param/return is a distinct Go type, not a bare int) plus the
	// named constants, prefixed by the Go enum type so they can't collide across enums. Enums are
	// grouped by OWNER (their enclosing class/namespace) into one file each — e.g. QStringConverter's
	// enums land in gen_qstringconverter_enums.go, right beside gen_qstringconverter.go — instead of
	// one monolithic gen_enums.go. Keeps the tree per-class and navigable, like miqt.
	owners := map[string]*strings.Builder{}
	var ownerOrder []string
	seenE := map[string]bool{}
	seenC := map[string]string{} // flat const name -> value (collision detection below)
	for _, e := range m.Enums {
		if rv.EnumPkg[e.USR] != pkg { // emit only enums owned by THIS package
			continue
		}
		gn := gen.GoEnumType(e.Name) // miqt-style: flat for Qt::, Class__Enum for class enums
		if gn == "" || seenE[gn] {
			continue
		}
		seenE[gn] = true
		owner := enumOwner(e.Name)
		eb, ok := owners[owner]
		if !ok {
			eb = &strings.Builder{}
			fmt.Fprintf(eb, "package %s\n\n", pkg)
			owners[owner] = eb
			ownerOrder = append(ownerOrder, owner)
		}
		// Pick the Go underlying type by value range: an unsigned-underlying enum (e.g.
		// QAnyStringView::Tag uses the top two bits -> 2^63, 0xC000...) overflows int64, so emit it as
		// uint64. int64 otherwise (covers negative enumerators).
		goType := "int64"
		for _, c := range e.Consts {
			if _, err := strconv.ParseInt(c.Value, 10, 64); err != nil {
				if _, err2 := strconv.ParseUint(c.Value, 10, 64); err2 == nil {
					goType = "uint64"
				}
			}
		}
		fmt.Fprintf(eb, "type %s %s\n", gn, goType)
		if len(e.Consts) == 0 {
			continue
		}
		eb.WriteString("\nconst (\n")
		for _, c := range e.Consts {
			cn := gen.GoEnumConst(e.Name, c.Name, e.Scoped) // miqt-style: flat DisplayRole / QFont__Monospace; scoped -> Class__Enum__Const
			if v, dup := seenC[cn]; dup {
				if v == c.Value {
					continue // same value re-declared elsewhere (alias) — harmless to skip
				}
				// Flat-name collision with a DIFFERENT value: silently dropping it (the old behavior)
				// shipped whichever constant came first. Re-qualify with the full enum-type segment;
				// if even that collides, report loudly rather than mis-ship.
				alt := gen.GoEnumConst(e.Name, c.Name, true)
				if v2, dup2 := seenC[alt]; dup2 {
					if v2 != c.Value {
						fmt.Fprintf(os.Stderr, "enum-const collision DROPPED: %s::%s = %s (name %s already = %s)\n",
							e.Name, c.Name, c.Value, alt, v2)
					}
					continue
				}
				cn = alt
			}
			seenC[cn] = c.Value
			fmt.Fprintf(eb, "\t%s %s = %s\n", cn, gn, c.Value)
		}
		eb.WriteString(")\n\n")
	}
	for _, owner := range ownerOrder {
		ofile := "global"
		if owner != "" {
			ofile = strings.ToLower(strings.ReplaceAll(owner, "::", "__"))
		}
		write(filepath.Join(outdir, "gen_"+ofile+"_enums.go"), owners[owner].String())
	}

	// The C-ABI header + C++ shims are identical for both backends; only the Go support files differ.
	write(filepath.Join(outdir, "cppgo_pch.h"), pchHeader)
	if rv.Backend == "purego" {
		// purego backend (the cgo-free backend): the support file + the two per-GOOS loader files
		// (unix dlopen / windows LoadLibrary) + the shim-exported allocator the support binds.
		write(filepath.Join(outdir, "gen_purego.go"), gen.PuregoSupport(pkg))
		unixSrc, winSrc := gen.PuregoSupportOS(pkg)
		write(filepath.Join(outdir, "gen_purego_unix.go"), unixSrc)
		write(filepath.Join(outdir, "gen_purego_windows.go"), winSrc)
		write(filepath.Join(cshimDir, "cppgo_alloc.cpp"), gen.PuregoAllocCpp)
		// RUN phase: the per-shape C->Go dispatch (collected across every EmitClass above). The Go
		// side installs purego.NewCallbacks; the C++ side defines the dispatch pointers the connect
		// shims / virtual overrides call. Written last so all shapes are present. Empty if no
		// supported signals/virtuals were emitted.
		if cbGo := gen.EmittedCbGo(pkg); cbGo != "" {
			write(filepath.Join(outdir, "gen_purego_cb.go"), cbGo)
			// cb.h/.cpp live in cshim/ (compiled + linked with the unit shims into libshim_<pkg>.so).
			write(filepath.Join(cshimDir, "gen_purego_cb.h"), gen.EmittedCbHeader())
			write(filepath.Join(cshimDir, "gen_purego_cb.cpp"), gen.EmittedCbCpp())
		}
	} else {
		write(filepath.Join(outdir, "gen_support.go"), strings.Replace(supportGo, "package genqt", "package "+pkg, 1))
		write(filepath.Join(outdir, "gen_callback.go"), strings.Replace(callbackGo, "package genqt", "package "+pkg, 1))
		// Per-shape cgo trampoline dispatchers (leg 2): collected across every EmitClass/EmitFunctions
		// call above, #included from every binding .go preamble. Written last so all shapes are present.
		write(filepath.Join(outdir, "cppgo_shims.h"), gen.EmittedShimHeader())
		// Leg-2 shape oracle (step 4): inert unless the build defines CPPGO_VERIFY.
		if vf := gen.EmittedVerifyFile(); vf != "" {
			write(filepath.Join(outdir, "cppgo_verify.c"), vf)
		}
	}
	// Coverage visibility: everything the generator declined to bind, counted by reason — always
	// printed, never silent. CPPGO_DROPLOG=<file> captures the per-symbol detail.
	if s := gen.DropSummary(); s != "" {
		fmt.Fprintf(os.Stderr, "package %s: %s\n", pkg, s)
	}
	// go.mod is written once at the module root by the build script (all packages share one module).
}

// enumOwner is the enum's enclosing scope: its qualified name minus the final ::segment.
// "QStringConverter::Encoding" -> "QStringConverter", "Qt::AlignmentFlag" -> "Qt", "Foo" -> "".
func enumOwner(name string) string {
	if i := strings.LastIndex(name, "::"); i >= 0 {
		return name[:i]
	}
	return ""
}

// write is write-if-changed: it skips files whose content is already identical, so an unchanged class
// keeps its mtime (calms ccache stat-checks, file watchers, editors) and a no-op regen touches nothing.
// Go's build cache is content-addressed so it doesn't need this, but the C/cgo toolchain partly does.
func write(path, content string) {
	if old, err := os.ReadFile(path); err == nil && string(old) == content {
		return
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		panic(err)
	}
}
