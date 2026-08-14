package gen

// The purego (cgo-free) Go-wrapper backend, emitted ALONGSIDE the cgo
// backend (rv.Backend=="purego"). The C++ method shims (gen_*.h / cshim) are reused
// as-is; only the Go side changes: each shim symbol is a package-level func var bound
// by purego.RegisterLibFunc and called directly; handles are uintptr.
//
// SLICE 2: root classes — constructors and instance methods whose params are scalars
// (incl. bool/enum), strings, or handles (*T), and whose return is void / scalar /
// string / handle. Statics, output params, containers, signals, virtuals, operators,
// field accessors, and derived classes are skipped (listed in a generated comment).

import (
	"fmt"
	"strings"
)

// PuregoSupport is the per-package support file: lazily load the shim library and expose
// reg() / symaddr() / b2i() / pgStr() to the generated wrappers. No cgo. The OS-specific
// pieces (dlopen vs LoadLibrary, symbol lookup, default library name) live in the two
// build-tagged companion files from PuregoSupportOS.
func PuregoSupport(pkg string) string {
	return fmt.Sprintf(`package %s

import (
	"os"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

var (
	_libOnce sync.Once
	_lib     uintptr
	pgFree   func(uintptr)
	pgMalloc func(uintptr) uintptr
)

// _load opens the shim library once (openShimLib / shimLibName are per-GOOS — see
// gen_purego_unix.go / gen_purego_windows.go). Override the path with $CPPGO_LIB_%s.
// The allocator pair is bound from the SHIM LIBRARY ITSELF (cppgo_alloc.cpp): Go frees
// exactly what the shims malloc'd — same C runtime by construction, no libc.so.6
// assumption, and it ports to Windows unchanged.
func _load() {
	_libOnce.Do(func() {
		path := os.Getenv(%q)
		if path == "" {
			path = shimLibName
		}
		h, err := openShimLib(path)
		if err != nil || h == 0 {
			msg := "cppgo/purego: cannot load " + path
			if err != nil {
				msg += ": " + err.Error()
			}
			panic(msg)
		}
		_lib = h
		purego.RegisterLibFunc(&pgFree, _lib, "cppgo_free")
		purego.RegisterLibFunc(&pgMalloc, _lib, "cppgo_malloc")
	})
}

func reg(p any, sym string) {
	_load()
	purego.RegisterLibFunc(p, _lib, sym)
}

// symaddr resolves a shim symbol's address once at init. Wrappers with register-only signatures call
// it directly via purego.SyscallN — no per-call reflection marshalling (several times cheaper than a
// RegisterLibFunc func var). Float-carrying signatures still go through reg() (SyscallN is
// integer-register only).
func symaddr(p *uintptr, sym string) {
	_load()
	a, err := shimSym(sym)
	if err != nil || a == 0 {
		panic("cppgo/purego: cannot resolve symbol " + sym)
	}
	*p = a
}

// b2i bridges Go bool <-> the C ABI int (0/1) representation of C++ bool.
func b2i(b bool) int32 {
	if b {
		return 1
	}
	return 0
}

// pgCString allocates a NUL-terminated C copy of s (caller frees via pgFree).
func pgCString(s string) uintptr {
	p := pgMalloc(uintptr(len(s) + 1))
	d := unsafe.Slice((*byte)(unsafe.Pointer(p)), len(s)+1)
	copy(d, s)
	d[len(s)] = 0
	return p
}

// pgStr copies a malloc'd UTF-8 C string of n bytes returned by a shim, then frees it.
func pgStr(p uintptr, n int64) string {
	if p == 0 || n <= 0 {
		return ""
	}
	s := string(unsafe.Slice((*byte)(unsafe.Pointer(p)), int(n)))
	if pgFree != nil {
		pgFree(p)
	}
	return s
}

// pgStrNoFree copies n UTF-8 bytes of a C string the caller does NOT own (a signal/virtual
// arg the C++ side keeps) — copy, never free.
func pgStrNoFree(p uintptr, n int64) string {
	if p == 0 || n <= 0 {
		return ""
	}
	return string(unsafe.Slice((*byte)(unsafe.Pointer(p)), int(n)))
}

// pgGoStr reads a NUL-terminated C string (e.g. a virtual's slot-key literal) without freeing.
func pgGoStr(p uintptr) string {
	if p == 0 {
		return ""
	}
	var n int
	for *(*byte)(unsafe.Pointer(p + uintptr(n))) != 0 {
		n++
	}
	return string(unsafe.Slice((*byte)(unsafe.Pointer(p)), n))
}

// Signal-callback registry: a Go func can't be passed to C, so it's stored by a sequential int64 id
// and the id crosses instead; the (per-shape) NewCallback dispatch looks it up. Slice-indexed (ids
// are dense, allocated here); a Disconnect nils the slot so the closure is collectable. RWMutex:
// deliveries only read.
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

// unregisterCallback releases the closure after Disconnect — without it, connect/disconnect-heavy
// long-running apps leak every callback ever registered.
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
// The inner list is a tiny linear scan (an instance typically overrides a handful of virtuals): no
// per-delivery hashing, no allocation.
type virtEnt struct {
	slot int64
	fn   any
}

// enableEnt flips one bit of the instance's C++-side override mask (__goOn) at registration time, so
// an UNREGISTERED virtual never crosses into Go — the C++ override falls straight through to the base.
type enableEnt struct {
	h      uintptr
	idxOf  func(slot int64) int32
	enable func(h uintptr, idx int32)
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

// registerInstance records how to enable a freshly constructed __Go instance's override-mask bits.
func registerInstance(id int64, h uintptr, idxOf func(int64) int32, enable func(uintptr, int32)) {
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

// unregisterInstance drops an instance's overrides + enable entry (called from Delete).
func unregisterInstance(id int64) {
	vMu.Lock()
	defer vMu.Unlock()
	if id >= 1 && int(id) <= len(vReg) {
		vReg[id-1] = nil
		vEnable[id-1] = enableEnt{}
	}
}
`, pkg, strings.ToUpper(pkg), "CPPGO_LIB_"+strings.ToUpper(pkg))
}

// PuregoSupportOS renders the two build-tagged loader files: unix (purego dlopen, RTLD_LOCAL)
// and windows (stdlib LoadLibrary/GetProcAddress — purego has no Dlopen there; its
// RegisterLibFunc/SyscallN/NewCallback work on the handle either way).
func PuregoSupportOS(pkg string) (unixSrc, winSrc string) {
	unixSrc = fmt.Sprintf(`//go:build !windows

package %s

import "github.com/ebitengine/purego"

// shimLibName is the default shim library (bare soname — the loader searches the usual paths;
// $CPPGO_LIB_%s overrides with an explicit path).
const shimLibName = "libshim_%s.so"

// openShimLib: RTLD_LAZY without RTLD_GLOBAL (= RTLD_LOCAL). Each package's shim library
// carries its OWN copy of the cppgo_* C->Go dispatch globals; under RTLD_GLOBAL those would
// interpose across packages (qt + webengine loaded together) and route every callback into
// whichever package init'd last — where the other package's objects aren't registered — so
// ALL signals/virtuals would silently no-op. RTLD_LOCAL keeps each lib's dispatch private.
func openShimLib(path string) (uintptr, error) {
	return purego.Dlopen(path, purego.RTLD_LAZY)
}

func shimSym(name string) (uintptr, error) {
	return purego.Dlsym(_lib, name)
}
`, pkg, strings.ToUpper(pkg), pkg)
	winSrc = fmt.Sprintf(`//go:build windows

package %s

import "syscall"

// shimLibName is the default shim DLL (searched via the standard DLL search order;
// $CPPGO_LIB_%s overrides with an explicit path). DLLs are per-module by construction —
// no RTLD_GLOBAL interposition risk on Windows.
const shimLibName = "shim_%s.dll"

func openShimLib(path string) (uintptr, error) {
	h, err := syscall.LoadLibrary(path)
	return uintptr(h), err
}

func shimSym(name string) (uintptr, error) {
	return syscall.GetProcAddress(syscall.Handle(_lib), name)
}
`, pkg, strings.ToUpper(pkg), pkg)
	return unixSrc, winSrc
}

// PuregoAllocCpp is the tiny allocator the shim library exports so the Go side frees with the
// exact C runtime the shims malloc'd from (see _load). Compiled into the shim library like any
// other cshim translation unit.
const PuregoAllocCpp = `// Code generated by cppgo; DO NOT EDIT.
#include <cstdlib>
extern "C" {
void* cppgo_malloc(size_t n) { return malloc(n); }
void cppgo_free(void* p) { free(p); }
}
`

// pgRegType maps a C-ABI type to the Go register type purego marshals it as.
func pgRegType(c string) string {
	c = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(c), "const "))
	if strings.HasSuffix(c, "*") {
		return "uintptr"
	}
	switch c {
	case "", "void":
		return ""
	case "bool", "int", "unsigned int", "unsigned":
		return "int32"
	case "long", "unsigned long":
		if dataModel == "llp64" {
			return "int32" // Windows: long is 4 bytes
		}
		return "int64"
	case "long long", "unsigned long long", "size_t", "qsizetype":
		return "int64"
	case "double":
		return "float64"
	case "float":
		return "float32"
	case "char", "signed char":
		return "int8"
	case "unsigned char":
		return "uint8"
	case "short", "unsigned short":
		return "int16"
	default:
		return "int32" // enums cross the ABI as int
	}
}

// pgSplitParam splits a Go param decl "name type" into (name, type).
func pgSplitParam(decl string) (name, typ string) {
	decl = strings.TrimSpace(decl)
	if i := strings.IndexByte(decl, ' '); i >= 0 {
		return decl[:i], strings.TrimSpace(decl[i+1:])
	}
	return decl, ""
}

// pgCType strips the trailing parameter name from a C decl ("const char* name" -> "const char*").
func pgCType(decl string) string {
	decl = strings.TrimSpace(decl)
	if i := strings.LastIndexByte(decl, ' '); i >= 0 {
		// keep the type, drop the last word, unless the last char is '*' attached to the type
		t := strings.TrimSpace(decl[:i])
		if t != "" {
			return t
		}
	}
	return decl
}

func pgIsScalarGo(goType string) bool {
	if goType == "" || goType == "string" {
		return false
	}
	return !strings.ContainsAny(goType, "*[]") && !strings.HasPrefix(goType, "map")
}

// pgPackable reports whether a method's call exceeds purego's 15-arg ceiling but can be routed
// through a packed-args shim instead: all inputs are simple scalars/pointers (1 register slot each,
// no doubles, strings, containers, or out-params) and the return is simple (no out-params). The args
// are passed as a single size_t array, so the call is just (self, arrayPtr). (In qt this is exactly
// QOpenGLExtraFunctions.GlCopyImageSubData — 16 slots.)
func pgPackable(m method) bool {
	if m.isCtor || m.isStat {
		return false // packed shim always has a receiver (void* self, void* __ap); no packable statics in qt
	}
	slots := 1 // self
	for _, p := range m.params {
		if p.goParam == "" || p.outRetType != "" || len(p.cParams) != 1 {
			return false // out-params / strings / containers aren't packable
		}
		reg := pgRegType(pgCType(p.cParams[0]))
		if reg == "" || reg == "float32" || reg == "float64" {
			return false // floats pass in separate registers / need bit-reinterpret — skip
		}
		slots++
	}
	if len(m.ret.cExtra) > 0 {
		return false // tuple/container/string returns add call slots
	}
	return slots > 15
}

// pgToReg converts a Go value of goType to its register representation (the inverse of pgScalarConv).
func pgToReg(goType, reg, expr string) string {
	switch {
	case goType == "bool":
		return "b2i(" + expr + ")"
	case goType == reg:
		return expr
	default:
		return reg + "(" + expr + ")"
	}
}

// pgSimpleRet converts the C return __r (a scalar/enum/bool or a record handle) to a Go value of
// goType. owned selects the owning constructor for a record handle (a by-value record return is a
// fresh heap copy the caller owns; a pointer/reference return is borrowed). ok=false => unsupported.
func pgSimpleRet(cRetType, goType string, owned bool) (varRet, expr string, ok bool) {
	cRetType = strings.TrimSpace(cRetType)
	if strings.HasSuffix(cRetType, "*") { // handle return
		if !strings.HasPrefix(goType, "*") {
			return "", "", false
		}
		t := goType[1:]
		if i := strings.IndexByte(t, '.'); i >= 0 { // cross-package -> exported UnsafeNew(Owned)
			ctor := "UnsafeNew" + t[i+1:]
			if owned {
				ctor += "Owned"
			}
			return "uintptr", t[:i] + "." + ctor + "(unsafe.Pointer(__r))", true
		}
		ctor := "bind" + t // same-package: bind (borrowed) / newowned (owned)
		if owned {
			ctor = "newowned" + t
		}
		return "uintptr", ctor + "(__r)", true
	}
	reg := pgRegType(cRetType)
	if reg == "" || reg == "uintptr" || goType == "" {
		return "", "", false
	}
	return reg, pgScalarConv(goType, reg, "__r"), true
}

// pgMethod renders one method/ctor for the purego backend. ok=false => skip (unsupported
// shape this slice). Returns the Go decl, and the (varName, varSig, symbol) to register.
// direct=true means the wrapper calls purego.SyscallN on a pre-resolved symbol ADDRESS (varName is a
// uintptr var registered via symaddr) — no per-call reflection. Signatures that carry a float in a
// VALUE position (arg or return) stay on RegisterLibFunc (SyscallN is integer-register only; float
// out-params are fine — they cross as pointers).
func pgMethod(cn string, m method, subclassable bool) (decl, varName, varSig, sym string, direct, ok bool) {
	// protCall/subclassRecv (protected & <X>Default base calls), operators, conversion operators, and
	// public field accessors all have a normal Go-wrapper shape — the C++ shim (shared by both backends)
	// encapsulates the operator/static_cast/field access, so the Go side just binds the symbol like any
	// other method.
	var wrapParams, varParamTypes, callArgs, dArgs, pre, post, outTypes, outExprs []string
	// Over-15-arg methods route their inputs through a packed size_t array (see pgPackable / the packed
	// C++ shim in emitCpp) so the purego call fits the 15-arg ceiling.
	packed := pgPackable(m)
	var packExprs []string
	// A ctor passes a leading override id ONLY when the class is subclassable (its ctor shim builds the
	// overridable __Go subclass); a non-subclassable class's ctor shim has no goid param (mirrors the cgo
	// subCtor distinction). Passing one anyway shifts every real arg — corrupting e.g. NewQVariant24.
	ctorGoid := m.isCtor && subclassable
	// Direct-call eligibility must be known up front — string marshalling differs between the two paths.
	direct = true
	for _, p := range m.params {
		for _, cp := range p.cParams {
			if r := pgRegType(pgCType(cp)); r == "float32" || r == "float64" {
				direct = false
			}
		}
	}
	if r := pgRegType(strings.TrimSpace(m.ret.cRetType)); !m.isCtor && (r == "float32" || r == "float64") {
		direct = false
	}
	// receiver / goid prefix
	switch {
	case m.isCtor:
		if ctorGoid {
			varParamTypes = append(varParamTypes, "int64")
			callArgs = append(callArgs, "__goid") // per-instance virtual-override id
			dArgs = append(dArgs, "uintptr(__goid)")
		}
	case m.isStat:
		// package-level func: no receiver, no self arg
	default:
		varParamTypes = append(varParamTypes, "uintptr")
		callArgs = append(callArgs, "this.h")
		dArgs = append(dArgs, "this.h")
		// Pin the receiver across the call: the wrapper is otherwise unreferenced once this.h is read,
		// so its GC finalizer could free the C++ object while the shim call is still executing.
		post = append(post, "runtime.KeepAlive(this)")
	}
	for _, p := range m.params {
		if packed { // every input is a simple scalar/pointer slot in the packed array
			name, goType := pgSplitParam(p.goParam)
			wrapParams = append(wrapParams, p.goParam)
			switch {
			case strings.HasPrefix(goType, "*"):
				packExprs = append(packExprs, "uintptr("+name+".UnsafePointer())")
				post = append(post, "runtime.KeepAlive("+name+")")
			case goType == "bool":
				packExprs = append(packExprs, "uintptr(b2i("+name+"))")
			default:
				packExprs = append(packExprs, "uintptr("+name+")")
			}
			continue
		}
		// Output parameter: a non-const scalar T*/T& the method writes (pure out, e.g. toInt(bool* ok),
		// or in/out ref, e.g. validate(QString&, int& pos)). Surfaced as an extra Go return; an in/out
		// ref ALSO takes the value as input (seeds the storage).
		if p.outRetType != "" {
			if len(p.cParams) != 1 {
				return "", "", "", "", false, false
			}
			reg := pgRegType(strings.TrimSuffix(pgCType(p.cParams[0]), "*"))
			if reg == "" || reg == "uintptr" {
				return "", "", "", "", false, false
			}
			nm := cParamName(p.cParams[0])
			local := nm + "_out"
			if p.goParam != "" { // in/out: seed the storage from the input value
				inName, inType := pgSplitParam(p.goParam)
				local = nm + "_io"
				wrapParams = append(wrapParams, p.goParam)
				pre = append(pre, local+" := "+pgToReg(inType, reg, inName))
			} else {
				pre = append(pre, "var "+local+" "+reg)
			}
			varParamTypes = append(varParamTypes, "uintptr")
			callArgs = append(callArgs, "uintptr(unsafe.Pointer(&"+local+"))")
			dArgs = append(dArgs, "uintptr(unsafe.Pointer(&"+local+"))")
			outTypes = append(outTypes, p.outRetType)
			outExprs = append(outExprs, pgScalarConv(p.outRetType, reg, local))
			continue
		}
		if p.goParam == "" { // an output we don't marshal (record/array out) — skip the whole method
			return "", "", "", "", false, false
		}
		name, goType := pgSplitParam(p.goParam)
		switch {
		case goType == "[]byte": // caller-provided raw byte buffer (char*/uchar* data)
			pre = append(pre,
				"var "+name+"_p uintptr",
				"if len("+name+") > 0 {\n\t\t"+name+"_p = uintptr(unsafe.Pointer(&"+name+"[0]))\n\t}")
			post = append(post, "runtime.KeepAlive("+name+")")
			wrapParams = append(wrapParams, p.goParam)
			varParamTypes = append(varParamTypes, "uintptr")
			callArgs = append(callArgs, name+"_p")
			dArgs = append(dArgs, name+"_p")
		case goType == "string":
			wrapParams = append(wrapParams, p.goParam)
			if len(p.cParams) >= 2 { // (char* data, long long len)
				if direct {
					// No copy: pass the string's own bytes (the shim copies immediately); keepAlive pins it.
					pre = append(pre,
						"var "+name+"_p uintptr",
						"if len("+name+") > 0 {\n\t\t"+name+"_p = uintptr(unsafe.Pointer(unsafe.StringData("+name+")))\n\t}")
					post = append(post, "runtime.KeepAlive("+name+")")
					varParamTypes = append(varParamTypes, "uintptr", "int64")
					dArgs = append(dArgs, name+"_p", "uintptr(len("+name+"))")
				} else {
					varParamTypes = append(varParamTypes, "string", "int64") // purego converts string->char*
					callArgs = append(callArgs, name, "int64(len("+name+"))")
				}
			} else { // single NUL-terminated const char*
				if direct {
					pre = append(pre, name+"_b := append([]byte("+name+"), 0)")
					post = append(post, "runtime.KeepAlive("+name+"_b)")
					varParamTypes = append(varParamTypes, "uintptr")
					dArgs = append(dArgs, "uintptr(unsafe.Pointer(&"+name+"_b[0]))")
				} else {
					varParamTypes = append(varParamTypes, "string")
					callArgs = append(callArgs, name)
				}
			}
		case strings.HasPrefix(goType, "[]"): // container param: build a C array, pass ptr+len
			elem := goType[2:]
			arr := name + "_arr"
			var arrType, elemBuild string
			switch {
			case elem == "string":
				arrType, elemBuild = "uintptr", "pgCString(__e)"
				post = append(post, "for __i := range "+arr+" {\n\t\tpgFree("+arr+"[__i])\n\t}")
			case strings.HasPrefix(elem, "*"): // handle elements
				arrType, elemBuild = "uintptr", "uintptr(__e.UnsafePointer())"
				// The C array holds raw handles; pin the WRAPPER slice too, or an element's finalizer
				// could free its C++ object mid-call.
				post = append(post, "runtime.KeepAlive("+name+")")
			default: // scalar elements
				if len(p.cParams) < 1 {
					return "", "", "", "", false, false
				}
				reg := pgRegType(strings.TrimSuffix(pgCType(p.cParams[0]), "*"))
				if reg == "" || reg == "uintptr" {
					return "", "", "", "", false, false
				}
				arrType = reg
				if elem == reg {
					elemBuild = "__e"
				} else {
					elemBuild = reg + "(__e)"
				}
			}
			pre = append(pre,
				arr+" := make([]"+arrType+", len("+name+"))",
				"for __i, __e := range "+name+" {\n\t\t"+arr+"[__i] = "+elemBuild+"\n\t}",
				"var "+name+"_p uintptr",
				"if len("+arr+") > 0 {\n\t\t"+name+"_p = uintptr(unsafe.Pointer(&"+arr+"[0]))\n\t}")
			post = append(post, "runtime.KeepAlive("+arr+")")
			wrapParams = append(wrapParams, p.goParam)
			varParamTypes = append(varParamTypes, "uintptr", "int64")
			callArgs = append(callArgs, name+"_p", "int64(len("+name+"))")
			dArgs = append(dArgs, name+"_p", "uintptr(len("+name+"))")
		case strings.HasPrefix(goType, "*"):
			wrapParams = append(wrapParams, p.goParam)
			varParamTypes = append(varParamTypes, "uintptr")
			callArgs = append(callArgs, "uintptr("+name+".UnsafePointer())")
			dArgs = append(dArgs, "uintptr("+name+".UnsafePointer())")
			post = append(post, "runtime.KeepAlive("+name+")")
		case pgIsScalarGo(goType):
			if len(p.cParams) != 1 {
				return "", "", "", "", false, false
			}
			reg := pgRegType(pgCType(p.cParams[0]))
			if reg == "" || reg == "uintptr" {
				return "", "", "", "", false, false // reference/pointer-backed scalar (in/out ref) — later slice
			}
			wrapParams = append(wrapParams, p.goParam)
			varParamTypes = append(varParamTypes, reg)
			switch {
			case goType == "bool":
				callArgs = append(callArgs, "b2i("+name+")")
				dArgs = append(dArgs, "uintptr(b2i("+name+"))")
			case goType == reg:
				callArgs = append(callArgs, name)
				dArgs = append(dArgs, "uintptr("+name+")")
			default:
				callArgs = append(callArgs, reg+"("+name+")")
				dArgs = append(dArgs, "uintptr("+name+")")
			}
		default:
			return "", "", "", "", false, false
		}
	}

	if packed { // collapse the per-arg slots into one size_t array; call is (self, &__pack[0])
		pre = append(pre, "__pack := []uintptr{"+strings.Join(packExprs, ", ")+"}")
		varParamTypes = append(varParamTypes, "uintptr")
		callArgs = append(callArgs, "uintptr(unsafe.Pointer(&__pack[0]))")
		dArgs = append(dArgs, "uintptr(unsafe.Pointer(&__pack[0]))")
	}

	ret := m.ret
	retC := strings.TrimSpace(ret.cRetType)
	grt := strings.TrimSpace(ret.goRetType)
	// A by-value record return is a fresh heap copy the caller owns (the C++ shim does `new T(...)`);
	// a pointer/reference return is borrowed. The plan signals it via the owning constructor it emits.
	owned := strings.Contains(ret.goReturn+strings.Join(ret.goPost, "\n"), "Owned(")
	var varRet string
	var mainTypes, mainExprs []string // the return's OWN value(s): 1 (simple), 2 (tuple), 0 (void)
	switch {
	case m.isCtor:
		varRet = "uintptr"
	case ret.isVoid:
		// no main return value
	case strings.HasPrefix(grt, "map["): // map[K]V: keys array (__r) + vals array + len (out-params)
		inner := grt[len("map["):]
		ci := strings.IndexByte(inner, ']')
		if ci < 0 || len(ret.cExtra) != 2 {
			return "", "", "", "", false, false
		}
		kGo, vGo := inner[:ci], inner[ci+1:]
		if !strings.HasPrefix(vGo, "*") { // only record-pointer values (the planner excludes string keys/vals)
			return "", "", "", "", false, false
		}
		kReg := pgRegType(strings.TrimSuffix(retC, "*"))
		if kReg == "" || kReg == "uintptr" {
			return "", "", "", "", false, false
		}
		varParamTypes = append(varParamTypes, "uintptr", "*int64") // &__vp (vals array), &__n (len)
		callArgs = append(callArgs, "uintptr(unsafe.Pointer(&__vp))", "&__n")
		dArgs = append(dArgs, "uintptr(unsafe.Pointer(&__vp))", "uintptr(unsafe.Pointer(&__n))")
		pre = append(pre, "var __vp uintptr", "var __n int64")
		varRet = "uintptr"
		t := vGo[1:]
		vconv := "bind" + t + "(__vs[__i])"
		if owned {
			vconv = "newowned" + t + "(__vs[__i])"
		}
		if i := strings.IndexByte(t, '.'); i >= 0 {
			ctor := "UnsafeNew" + t[i+1:]
			if owned {
				ctor += "Owned"
			}
			vconv = t[:i] + "." + ctor + "(unsafe.Pointer(__vs[__i]))"
		}
		post = append(post,
			"__ks := unsafe.Slice((*"+kReg+")(unsafe.Pointer(__r)), int(__n))",
			"__vs := unsafe.Slice((*uintptr)(unsafe.Pointer(__vp)), int(__n))",
			"__out := make("+grt+", int(__n))",
			"for __i := 0; __i < int(__n); __i++ {\n\t\t__out["+pgScalarConv(kGo, kReg, "__ks[__i]")+"] = "+vconv+"\n\t}",
			"pgFree(__r)",
			"pgFree(__vp)")
		mainTypes, mainExprs = []string{grt}, []string{"__out"}
	case strings.HasPrefix(grt, "[]"): // sequence container return: a C array + out_len
		elem := grt[2:]
		varParamTypes = append(varParamTypes, "*int64")
		callArgs = append(callArgs, "&__n")
		dArgs = append(dArgs, "uintptr(unsafe.Pointer(&__n))")
		pre = append(pre, "var __n int64")
		varRet = "uintptr"
		switch {
		case elem == "string": // char** array: read + free each element, then the array
			post = append(post,
				"__out := make([]string, __n)",
				"__sl := unsafe.Slice((*uintptr)(unsafe.Pointer(__r)), int(__n))",
				"for __i := range __out {\n\t\t__out[__i] = pgGoStr(__sl[__i])\n\t\tpgFree(__sl[__i])\n\t}",
				"pgFree(__r)")
		case strings.HasPrefix(elem, "*"): // []*handle
			t := strings.TrimPrefix(elem, "*")
			conv := "bind" + t + "(__sl[__i])"
			if owned {
				conv = "newowned" + t + "(__sl[__i])"
			}
			if i := strings.IndexByte(t, '.'); i >= 0 {
				ctor := "UnsafeNew" + t[i+1:]
				if owned {
					ctor += "Owned"
				}
				conv = t[:i] + "." + ctor + "(unsafe.Pointer(__sl[__i]))"
			}
			post = append(post,
				"__out := make("+grt+", __n)",
				"__sl := unsafe.Slice((*uintptr)(unsafe.Pointer(__r)), int(__n))",
				"for __i := range __out {\n\t\t__out[__i] = "+conv+"\n\t}",
				"pgFree(__r)")
		default: // []scalar
			reg := pgRegType(strings.TrimSuffix(retC, "*"))
			if reg == "" || reg == "uintptr" {
				return "", "", "", "", false, false
			}
			conv := "__sl[__i]"
			if elem != reg {
				conv = elem + "(__sl[__i])"
			}
			post = append(post,
				"__out := make("+grt+", __n)",
				"__sl := unsafe.Slice((*"+reg+")(unsafe.Pointer(__r)), int(__n))",
				"for __i := range __out {\n\t\t__out[__i] = "+conv+"\n\t}",
				"pgFree(__r)")
		}
		mainTypes, mainExprs = []string{grt}, []string{"__out"}
	case strings.HasPrefix(grt, "("): // (A, B) tuple (optional / pair): A via __r, B via a scalar out-param
		if len(ret.cExtra) != 1 {
			return "", "", "", "", false, false
		}
		inner := strings.TrimSuffix(strings.TrimPrefix(grt, "("), ")")
		parts := strings.SplitN(inner, ", ", 2)
		if len(parts) != 2 {
			return "", "", "", "", false, false
		}
		vr, aExpr, ok2 := pgSimpleRet(retC, parts[0], owned)
		if !ok2 {
			return "", "", "", "", false, false
		}
		bReg := pgRegType(strings.TrimSuffix(pgCType(ret.cExtra[0]), "*"))
		if bReg == "" || bReg == "uintptr" {
			return "", "", "", "", false, false
		}
		varParamTypes = append(varParamTypes, "uintptr")
		callArgs = append(callArgs, "uintptr(unsafe.Pointer(&__b))")
		dArgs = append(dArgs, "uintptr(unsafe.Pointer(&__b))")
		pre = append(pre, "var __b "+bReg)
		varRet = vr
		mainTypes = []string{parts[0], parts[1]}
		mainExprs = []string{aExpr, pgScalarConv(parts[1], bReg, "__b")}
	case grt == "string": // char* + out_len
		varParamTypes = append(varParamTypes, "*int64")
		callArgs = append(callArgs, "&__n")
		dArgs = append(dArgs, "uintptr(unsafe.Pointer(&__n))")
		pre = append(pre, "var __n int64")
		varRet = "uintptr"
		mainTypes, mainExprs = []string{"string"}, []string{"pgStr(__r, __n)"}
	default: // scalar / enum / bool, or a record handle
		vr, expr, ok2 := pgSimpleRet(retC, grt, owned)
		if !ok2 {
			return "", "", "", "", false, false
		}
		varRet = vr
		mainTypes, mainExprs = []string{grt}, []string{expr}
	}

	if !packed && len(varParamTypes) > 15 {
		return "", "", "", "", false, false // purego maxArgs=15 — unpackable many-arg funcs stay skipped
	}
	sym = m.cFunc
	var call, capture string
	if direct {
		// Pre-resolved symbol address + purego.SyscallN: no per-call reflection. Pointer-to-local args
		// are converted INSIDE the call expression (SyscallN is //go:uintptrescapes, which pins them).
		varName = "addr_" + m.cFunc
		varSig = "uintptr"
		call = "purego.SyscallN(" + strings.Join(append([]string{varName}, dArgs...), ", ") + ")"
		if varRet == "uintptr" || varRet == "" {
			capture = "__r, _, _ := " + call
		} else {
			// The raw register's upper bits are undefined for sub-64-bit C returns — truncate first.
			capture = "__r0, _, _ := " + call + "\n\t__r := " + pgTrunc(varRet, "__r0")
		}
	} else {
		varName = "pg_" + m.cFunc
		varSig = "func(" + strings.Join(varParamTypes, ", ") + ")"
		if varRet != "" {
			varSig += " " + varRet
		}
		call = varName + "(" + strings.Join(callArgs, ", ") + ")"
		capture = "__r := " + call
	}

	var b strings.Builder
	if m.isCtor {
		fmt.Fprintf(&b, "func %s(%s) *%s {\n", m.goName, strings.Join(wrapParams, ", "), cn)
		if ctorGoid { // subclassable: per-instance override id -> ctor shim, stamped on the wrapper
			b.WriteString("\t__goid := nextVirtID()\n")
		}
		pgWriteStmts(&b, pre)
		if direct {
			fmt.Fprintf(&b, "\t__r, _, _ := %s\n\t__o := newowned%s(__r)\n", call, cn)
		} else {
			fmt.Fprintf(&b, "\t__o := newowned%s(%s)\n", cn, call)
		}
		pgWriteStmts(&b, post)
		if ctorGoid {
			b.WriteString("\t__o.UnsafeSetGoID(__goid)\n")
			// Wire the instance's C++ override mask so a later On<X> enables exactly its bit.
			fmt.Fprintf(&b, "\tregisterInstance(__goid, __o.h, %s_virtIdx, %s_enable)\n", cn, cn)
		}
		b.WriteString("\treturn __o\n}\n\n")
		return b.String(), varName, varSig, sym, direct, true
	}
	recv := "(this *" + cn + ") "
	if m.isStat {
		recv = ""
	}
	// Return signature = the method's own return value(s) + each output param, parenthesised when >1.
	allTypes := append(append([]string{}, mainTypes...), outTypes...)
	allExprs := append(append([]string{}, mainExprs...), outExprs...)
	retDecl := ""
	if len(allTypes) == 1 {
		retDecl = " " + allTypes[0]
	} else if len(allTypes) > 1 {
		retDecl = " (" + strings.Join(allTypes, ", ") + ")"
	}
	fmt.Fprintf(&b, "func %s%s(%s)%s {\n", recv, m.goName, strings.Join(wrapParams, ", "), retDecl)
	pgWriteStmts(&b, pre)
	if len(mainExprs) == 0 { // void return value (may still have output-param returns)
		fmt.Fprintf(&b, "\t%s\n", call)
		pgWriteStmts(&b, post)
		if len(allExprs) > 0 {
			fmt.Fprintf(&b, "\treturn %s\n", strings.Join(allExprs, ", "))
		}
	} else {
		fmt.Fprintf(&b, "\t%s\n", capture)
		pgWriteStmts(&b, post)
		fmt.Fprintf(&b, "\treturn %s\n", strings.Join(allExprs, ", "))
	}
	b.WriteString("}\n\n")
	return b.String(), varName, varSig, sym, direct, true
}

// pgTrunc converts a raw SyscallN register (uintptr) to the reg-typed value the conversions below
// expect. Sub-64-bit integer returns must truncate through the unsigned same-width type first — the
// upper register bits are undefined for a C int/short/char return.
func pgTrunc(reg, expr string) string {
	switch reg {
	case "int64":
		return "int64(" + expr + ")"
	case "uint64":
		return "uint64(" + expr + ")"
	case "int32":
		return "int32(uint32(" + expr + "))"
	case "uint32":
		return "uint32(" + expr + ")"
	case "int16":
		return "int16(uint16(" + expr + "))"
	case "uint16":
		return "uint16(" + expr + ")"
	case "int8":
		return "int8(uint8(" + expr + "))"
	case "uint8":
		return "uint8(" + expr + ")"
	}
	return expr
}

func pgWriteStmts(b *strings.Builder, ss []string) {
	for _, s := range ss {
		b.WriteString("\t" + s + "\n")
	}
}

func pgScalarConv(goType, regType, expr string) string {
	switch {
	case goType == "bool":
		return expr + " != 0"
	case goType == regType:
		return expr
	default:
		return goType + "(" + expr + ")"
	}
}

// emitGoPurego renders the Go wrapper for a (root) class using the purego backend.
func emitGoPurego(pkg, cn, baseImport string, ms []method, sigs []signal, virts []vmethod, isQObject, parentCtor, parentOpt, publicDtor bool, qobjType string, base baseEmit, chain []baseEmit) string {
	var body strings.Builder
	var vars, regs, skipped []string
	addVar := func(name, sig, sym string) {
		vars = append(vars, "\t"+name+" "+sig)
		regs = append(regs, fmt.Sprintf("\treg(&%s, %q)", name, sym))
	}
	addAddr := func(name, sym string) { // direct-call symbol address (resolved once, no reflection)
		vars = append(vars, "\t"+name+" uintptr")
		regs = append(regs, fmt.Sprintf("\tsymaddr(&%s, %q)", name, sym))
	}

	// struct + bind: a root carries the handle + virtual-override id; a derived class EMBEDS its
	// primary base (miqt-style) so Go method promotion inherits the base's methods. The embedded
	// base is built from the base-subobject pointer(s): a 2+-level same-package chain fetches ALL of
	// them in ONE <cn>_chain crossing; otherwise the per-level <cn>_base cast shim.
	switch {
	case !base.has:
		fmt.Fprintf(&body, "type %s struct {\n\th   uintptr\n\tvid int64 // virtual-override id (0 for non-subclass instances)\n}\n\n", cn)
		fmt.Fprintf(&body, "func (this *%s) UnsafeGoID() int64     { return this.vid }\n", cn)
		fmt.Fprintf(&body, "func (this *%s) UnsafeSetGoID(id int64) { this.vid = id }\n\n", cn)
		fmt.Fprintf(&body, "func bind%s(h uintptr) *%s {\n\tif h == 0 {\n\t\treturn nil\n\t}\n\treturn &%s{h: h}\n}\n\n", cn, cn, cn)
	case len(chain) >= 2:
		addAddr("addr_"+cn+"_chain", cn+"_chain")
		k := len(chain)
		last := chain[k-1]
		var inner string
		if last.cross {
			inner = fmt.Sprintf("%sUnsafeNew%s(unsafe.Pointer(__c[%d]))", last.pfx, last.bare, k-1)
		} else {
			inner = fmt.Sprintf("&%s{h: __c[%d]}", last.bare, k-1)
		}
		for i := k - 2; i >= 0; i-- {
			inner = fmt.Sprintf("&%s{h: __c[%d], %s: %s}", chain[i].bare, i, chain[i+1].field, inner)
		}
		fmt.Fprintf(&body, "type %s struct {\n\th uintptr\n\t*%s\n}\n\n", cn, base.goType)
		fmt.Fprintf(&body, "func bind%s(h uintptr) *%s {\n\tif h == 0 {\n\t\treturn nil\n\t}\n\tvar __c [%d]uintptr\n\tpurego.SyscallN(addr_%s_chain, h, uintptr(unsafe.Pointer(&__c[0])))\n\treturn &%s{h: h, %s: %s}\n}\n\n",
			cn, cn, k, cn, cn, chain[0].field, inner)
	default:
		addVar("pg_"+base.cFunc, "func(uintptr) uintptr", base.cFunc) // base-subobject cast
		baseBuild := "bind" + base.bare + "(pg_" + base.cFunc + "(h))"
		if base.cross {
			baseBuild = base.pfx + "UnsafeNew" + base.bare + "(unsafe.Pointer(pg_" + base.cFunc + "(h)))"
		}
		fmt.Fprintf(&body, "type %s struct {\n\th uintptr\n\t*%s\n}\n\n", cn, base.goType)
		fmt.Fprintf(&body, "func bind%s(h uintptr) *%s {\n\tif h == 0 {\n\t\treturn nil\n\t}\n\treturn &%s{h: h, %s: %s}\n}\n\n", cn, cn, cn, base.field, baseBuild)
	}
	if isQObject || !publicDtor {
		fmt.Fprintf(&body, "func newowned%s(h uintptr) *%s { return bind%s(h) }\n\n", cn, cn, cn)
	} else {
		fmt.Fprintf(&body, "func newowned%s(h uintptr) *%s {\n\to := bind%s(h)\n\tif o != nil {\n\t\truntime.SetFinalizer(o, (*%s).Delete)\n\t}\n\treturn o\n}\n\n", cn, cn, cn, cn)
	}
	// chandle stays uintptr (internal, for the purego shim calls). The EXPORTED accessors use
	// unsafe.Pointer to match the cgo backend's API exactly (drop-in for consumers like mainthread).
	fmt.Fprintf(&body, "func (this *%s) chandle() uintptr {\n\tif this == nil {\n\t\treturn 0\n\t}\n\treturn this.h\n}\n\n", cn)
	fmt.Fprintf(&body, "func (this *%s) UnsafePointer() unsafe.Pointer { return unsafe.Pointer(this.chandle()) }\n", cn)
	fmt.Fprintf(&body, "func UnsafeNew%s(h unsafe.Pointer) *%s { return bind%s(uintptr(h)) }\n", cn, cn, cn)
	fmt.Fprintf(&body, "func UnsafeNew%sOwned(h unsafe.Pointer) *%s { return newowned%s(uintptr(h)) }\n\n", cn, cn, cn)
	// UnsafeRelease drops this wrapper's handle AND every embedded base wrapper's — Delete calls it
	// after freeing, so no stale base handle survives (previously a post-Delete promoted-method call
	// was a use-after-free through the still-live embedded base).
	if base.has {
		fmt.Fprintf(&body, "func (this *%s) UnsafeRelease() {\n\tthis.h = 0\n\tif this.%s != nil {\n\t\tthis.%s.UnsafeRelease()\n\t}\n}\n\n", cn, base.field, base.field)
	} else {
		fmt.Fprintf(&body, "func (this *%s) UnsafeRelease() { this.h = 0 }\n\n", cn)
	}

	for _, m := range ms {
		decl, vn, vs, sym, direct, ok := pgMethod(cn, m, len(virts) > 0)
		if !ok {
			skipped = append(skipped, m.goName)
			continue
		}
		if direct {
			addAddr(vn, sym)
		} else {
			addVar(vn, vs, sym)
		}
		body.WriteString(decl)
	}

	// Signals (receive): On<Sig>(fn) registers the Go closure by id and hands the id to the
	// connect shim. The C->Go dispatch (per-shape NewCallback) lands with the connect-shim
	// re-emit; this Go side compiles + registers now (matches the cgo connect-shim signature).
	for _, s := range sigs {
		_, wrapperDecls, _, vals, ok := pgSigMarshal(s)
		if !ok { // unsupported callback shape — skipped in lockstep with the C++ connect shim
			skipped = append(skipped, s.connectGo)
			continue
		}
		exposed := s.exposed
		if exposed > len(s.args) {
			exposed = len(s.args)
		}
		var goArgs []string
		for _, a := range s.args[:exposed] {
			goArgs = append(goArgs, a.goType)
		}
		addVar("pg_"+s.connectC, "func(uintptr, int64)", s.connectC)
		// The stored callback is a wrapper: the generic dispatch hands it raw register values,
		// the wrapper converts them and calls the user's typed fn.
		wrapper := "func(" + strings.Join(wrapperDecls, ", ") + ") { fn(" + strings.Join(vals, ", ") + ") }"
		fmt.Fprintf(&body, "func (this *%s) %s(fn func(%s)) int64 {\n\tid := registerCallback(%s)\n\tpg_%s(this.h, id)\n\truntime.KeepAlive(this)\n\treturn id\n}\n\n",
			cn, s.connectGo, strings.Join(goArgs, ", "), wrapper, s.connectC)
	}
	// Disconnect(handle) tears down one connection by its id (the C++ __disconnect shim is emitted
	// for both backends whenever the class has any signal), then releases the registered Go closure.
	if len(sigs) > 0 {
		addVar("pg_"+cn+"__disconnect", "func(int64)", cn+"__disconnect")
		fmt.Fprintf(&body, "func (this *%s) Disconnect(handle int64) {\n\tpg_%s__disconnect(handle)\n\tunregisterCallback(handle)\n}\n\n", cn, cn)
	}

	// Constructors the normal marshalling can't bind: app classes (int& argc, char** argv) and
	// abstract classes (no public ctor -> the override-subclass ctor). Mirrors the cgo emitGo
	// special-ctor block; concrete subclassable classes get their New<Cls> from pgMethod.
	hasCtor := false
	for _, m := range ms {
		if m.isCtor {
			hasCtor = true
			break
		}
	}
	emitAppCtor := func(withGoid bool) {
		sig := "func(uintptr, uintptr) uintptr"
		if withGoid {
			sig = "func(int64, uintptr, uintptr) uintptr"
		}
		addVar("pg_"+cn+"_new_args", sig, cn+"_new_args")
		fmt.Fprintf(&body, "func New%s(args []string) *%s {\n", cn, cn)
		body.WriteString("\targc := pgMalloc(unsafe.Sizeof(int32(0)))\n")
		body.WriteString("\t*(*int32)(unsafe.Pointer(argc)) = int32(len(args))\n")
		body.WriteString("\targv := pgMalloc(uintptr(len(args)+1) * unsafe.Sizeof(uintptr(0)))\n")
		body.WriteString("\t__av := unsafe.Slice((*uintptr)(unsafe.Pointer(argv)), len(args)+1)\n")
		body.WriteString("\tfor i := range args {\n\t\t__av[i] = pgCString(args[i])\n\t}\n")
		body.WriteString("\t__av[len(args)] = 0\n")
		body.WriteString("\truntime.LockOSThread()\n")
		if withGoid {
			fmt.Fprintf(&body, "\t__goid := nextVirtID()\n\to := newowned%s(pg_%s_new_args(__goid, argc, argv))\n\to.UnsafeSetGoID(__goid)\n\tregisterInstance(__goid, o.h, %s_virtIdx, %s_enable)\n\treturn o\n}\n\n", cn, cn, cn, cn)
		} else {
			fmt.Fprintf(&body, "\treturn newowned%s(pg_%s_new_args(argc, argv))\n}\n\n", cn, cn)
		}
	}
	if len(virts) > 0 {
		// Per-class mask plumbing (see the support file's registerInstance): <cn>_virtIdx maps a global
		// slot id to this class's __Go override-mask index; <cn>_enable flips that bit C++-side.
		addVar("pg_"+cn+"__set_override", "func(uintptr, int32)", cn+"__set_override")
		fmt.Fprintf(&body, "func %s_virtIdx(slot int64) int32 {\n\tswitch slot {\n", cn)
		for _, v := range virts {
			fmt.Fprintf(&body, "\tcase %d:\n\t\treturn %d\n", v.slotID, v.idx)
		}
		fmt.Fprint(&body, "\t}\n\treturn -1\n}\n\n")
		fmt.Fprintf(&body, "func %s_enable(h uintptr, idx int32) { pg_%s__set_override(h, idx) }\n\n", cn, cn)
		if isAppClass(cn) {
			emitAppCtor(true)
		} else if !hasCtor {
			if parentCtor && parentOpt {
				addVar("pg_"+cn+"__Go_new", "func(int64, uintptr) uintptr", cn+"__Go_new")
				fmt.Fprintf(&body, "func New%s() *%s {\n\tid := nextVirtID()\n\to := newowned%s(pg_%s__Go_new(id, 0))\n\to.UnsafeSetGoID(id)\n\tregisterInstance(id, o.h, %s_virtIdx, %s_enable)\n\treturn o\n}\n\n", cn, cn, cn, cn, cn, cn)
				fmt.Fprintf(&body, "func New%s2(parent %s) *%s {\n\tid := nextVirtID()\n\to := newowned%s(pg_%s__Go_new(id, uintptr(parent.UnsafePointer())))\n\to.UnsafeSetGoID(id)\n\tregisterInstance(id, o.h, %s_virtIdx, %s_enable)\n\treturn o\n}\n\n", cn, qobjType, cn, cn, cn, cn, cn)
			} else if parentCtor {
				addVar("pg_"+cn+"__Go_new", "func(int64, uintptr) uintptr", cn+"__Go_new")
				fmt.Fprintf(&body, "func New%s(parent %s) *%s {\n\tid := nextVirtID()\n\to := newowned%s(pg_%s__Go_new(id, uintptr(parent.UnsafePointer())))\n\to.UnsafeSetGoID(id)\n\tregisterInstance(id, o.h, %s_virtIdx, %s_enable)\n\treturn o\n}\n\n", cn, qobjType, cn, cn, cn, cn, cn)
			} else {
				addVar("pg_"+cn+"__Go_new", "func(int64) uintptr", cn+"__Go_new")
				fmt.Fprintf(&body, "func New%s() *%s {\n\tid := nextVirtID()\n\to := newowned%s(pg_%s__Go_new(id))\n\to.UnsafeSetGoID(id)\n\tregisterInstance(id, o.h, %s_virtIdx, %s_enable)\n\treturn o\n}\n\n", cn, cn, cn, cn, cn, cn)
			}
		}
	}

	// Virtuals (override from Go): On<Virtual>(fn) registers the Go impl against the instance's
	// override id. (Compiles + registers now; the C++ override shim re-emit + per-shape NewCallback
	// dispatch land in the run phase.)
	for _, v := range virts {
		_, wrapperDecls, _, vals, kind, _, retReg, convFmt, ok := pgVirtMarshal(v)
		if !ok { // unsupported callback shape — skipped in lockstep with the C++ override
			skipped = append(skipped, v.onGo)
			continue
		}
		var goTypes []string
		for _, a := range v.args {
			goTypes = append(goTypes, a.goType)
		}
		fnType := "func(" + strings.Join(goTypes, ", ") + ")"
		if !v.isVoid {
			fnType += " " + v.goRet
		}
		wrapper := pgVirtWrapper(v, wrapperDecls, vals, kind, retReg, convFmt)
		fmt.Fprintf(&body, "func (this *%s) %s(fn %s) {\n\tregisterVirt(this.UnsafeGoID(), %d, %s) // slot %q\n}\n\n", cn, v.onGo, fnType, v.slotID, wrapper, v.slotKey)
	}

	// Non-subclassable app class (QApplication, ...): the plain New<App>(args) the virts block skipped.
	if len(virts) == 0 && isAppClass(cn) {
		emitAppCtor(false)
	}

	// Delete mirrors the cgo backend: cancel the finalizer, drop registry entries, free, then release
	// the WHOLE wrapper chain (embedded base handles included — the post-Delete UAF fix).
	if publicDtor {
		addVar("pg_"+cn+"_delete", "func(uintptr)", cn+"_delete")
		fmt.Fprintf(&body, "func (this *%s) Delete() {\n\truntime.SetFinalizer(this, nil)\n\tif __id := this.UnsafeGoID(); __id != 0 {\n\t\tunregisterInstance(__id)\n\t}\n\tif this.h != 0 {\n\t\tpg_%s_delete(this.h)\n\t}\n\tthis.UnsafeRelease()\n}\n", cn, cn)
	} else {
		fmt.Fprintf(&body, "func (this *%s) Delete() {\n\truntime.SetFinalizer(this, nil)\n\tif __id := this.UnsafeGoID(); __id != 0 {\n\t\tunregisterInstance(__id)\n\t}\n\tthis.UnsafeRelease()\n}\n", cn)
	}

	var out strings.Builder
	fmt.Fprintf(&out, "package %s\n\n", pkg)
	out.WriteString("import (\n\t\"unsafe\"\n")
	if strings.Contains(body.String(), "math.") {
		out.WriteString("\t\"math\"\n")
	}
	if strings.Contains(body.String(), "runtime.") {
		out.WriteString("\t\"runtime\"\n")
	}
	if strings.Contains(body.String(), "purego.") {
		out.WriteString("\n\t\"github.com/ebitengine/purego\"\n")
	}
	if baseImport != "" && strings.Contains(body.String(), "qt.") {
		fmt.Fprintf(&out, "\tqt %q\n", baseImport)
	}
	out.WriteString(")\n\n")
	if len(skipped) > 0 {
		fmt.Fprintf(&out, "// purego: unsupported shapes skipped (map returns, >15-arg / >9-arg-callback, exotic): %s\n\n", strings.Join(skipped, ", "))
	}
	if len(vars) > 0 {
		fmt.Fprintf(&out, "var (\n%s\n)\n\nfunc init() {\n%s\n}\n\n", strings.Join(vars, "\n"), strings.Join(regs, "\n"))
	}
	out.WriteString(body.String())
	return out.String()
}
