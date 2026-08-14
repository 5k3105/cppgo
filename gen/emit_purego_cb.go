package gen

// C->Go callback dispatch for the purego backend.
//
// The cgo backend's connect-lambdas and virtual overrides call fixed `//export` symbols
// (cppgoOnX) that don't exist in a plain .so. The purego backend instead routes every
// signal/virtual through a per-SHAPE dispatch function pointer, set once from Go via
// purego.NewCallback. Shapes are bucketed by C signature (collapse-by-id), so the
// ~thousands of call sites map onto a few dozen NewCallbacks (well under purego's ceiling).
//
// Correctness with collapse: the dispatch is fully GENERIC (raw register types in/out).
// All type-specific marshalling lives in a wrapper closure built at On<X>/connect time in
// emitGoPurego and stored in the callback/virt registry — so two signals that share a C
// shape but differ in Go type each carry their own conversion. The dispatch only does the
// id/slot lookup, sets the `handled` flag, and forwards raw register values.

import (
	"fmt"
	"sort"
	"strings"
)

// cbShape is one distinct C->Go dispatch signature. kind "sig": void(long long id, args...).
// kind "virt": ret(long long goid, const char* slot, args..., int* handled).
type cbShape struct {
	kind    string   // "sig" | "virt"
	argRegs []string // Go register types of the marshalled args (a0..aN)
	retReg  string   // Go register return type ("" for void)
}

var cbShapes = map[string]cbShape{} // token -> shape (package-global, like shimShapes)

// cParamName extracts the identifier from a C param decl ("const char* s" -> "s").
func cParamName(decl string) string {
	decl = strings.TrimSpace(decl)
	if i := strings.LastIndexAny(decl, " *&"); i >= 0 {
		return decl[i+1:]
	}
	return decl
}

// pgIsRecordType reports whether a pointee Go type is a bound record (cross-package "qt.QX"
// or a same-package "QX") rather than a Go-fixed scalar alias (int32/float64/...).
func pgIsRecordType(inner string) bool {
	if strings.Contains(inner, ".") {
		return true
	}
	return inner != "" && inner[0] >= 'A' && inner[0] <= 'Z'
}

// pgUnsafeNewExpr builds the borrowed-handle constructor call for a record pointee.
func pgUnsafeNewExpr(inner, ptr string) string {
	if i := strings.IndexByte(inner, '.'); i >= 0 {
		return inner[:i] + ".UnsafeNew" + inner[i+1:] + "(" + ptr + ")"
	}
	return "UnsafeNew" + inner + "(" + ptr + ")"
}

func pgZero(reg string) string {
	if reg == "" {
		return ""
	}
	return "0" // untyped 0 converts to any numeric reg type (int32/int64/float64/uintptr)
}

// cbRetToUintptr converts a reg-typed wrapper result to the single uintptr every dispatch returns.
// Sub-64-bit values go through their unsigned same-width type so the low bits carry the exact
// pattern; the C++ caller's function pointer is typed with the real (narrower) return and reads
// only those bits.
func cbRetToUintptr(reg, expr string) string {
	switch reg {
	case "uintptr":
		return expr
	case "int32":
		return "uintptr(uint32(" + expr + "))"
	case "int16":
		return "uintptr(uint16(" + expr + "))"
	case "int8":
		return "uintptr(uint8(" + expr + "))"
	}
	return "uintptr(" + expr + ")" // int64/uint64/uint32/uint16/uint8
}

// pgArgMarshal renders one C->Go callback arg: the C param types, the register-typed wrapper
// param decls (named from the C params), their reg types, and the Go expr converting them to
// the binding's Go value. ok=false => unsupported shape (defer).
//
// FLOAT args cross the dispatch as raw IEEE bits in an INTEGER register (mirroring the scalarf
// return path): Windows' syscall.NewCallback (which purego delegates to there) rejects float
// callback arguments outright, and one integer-only ABI keeps the dispatch identical on every
// platform. The C++ caller bit-casts (see pgFloatBits); the wrapper reconstitutes via math.
func pgArgMarshal(a sigArg) (cTypes, regDecls, regTypes []string, goVal string, ok bool) {
	floatArg := "" // "float32"/"float64" when this arg is a float carried as bits
	for _, cp := range a.cParams {
		ct := cTypeOf(cp)
		reg := pgRegType(ct)
		if reg == "" {
			return nil, nil, nil, "", false
		}
		if reg == "float64" || reg == "float32" {
			floatArg = reg
			if reg == "float64" {
				ct, reg = "unsigned long long", "uintptr"
			} else {
				ct, reg = "unsigned int", "uintptr"
			}
		}
		cTypes = append(cTypes, ct)
		regTypes = append(regTypes, reg)
		regDecls = append(regDecls, cParamName(cp)+" "+reg)
	}
	switch {
	case a.goType == "string":
		if len(a.cParams) < 2 {
			return nil, nil, nil, "", false
		}
		goVal = "pgStrNoFree(" + cParamName(a.cParams[0]) + ", " + cParamName(a.cParams[1]) + ")"
	case a.goType == "unsafe.Pointer":
		goVal = "unsafe.Pointer(" + cParamName(a.cParams[0]) + ")"
	case strings.HasPrefix(a.goType, "[]*"): // container arg -> []*Elem (borrowed, call-duration only; gap 4)
		inner := a.goType[3:]
		arr := cParamName(a.cParams[0])
		n := cParamName(a.cParams[1])
		goVal = "func() " + a.goType + " { if " + arr + " == 0 || " + n + " == 0 { return nil }; __sl := unsafe.Slice((*unsafe.Pointer)(unsafe.Pointer(" + arr + ")), int(" + n + ")); __o := make(" + a.goType + ", int(" + n + ")); for __i := range __o { __o[__i] = " + pgUnsafeNewExpr(inner, "__sl[__i]") + " }; return __o }()"
	case strings.HasPrefix(a.goType, "*"):
		inner := a.goType[1:]
		nm := cParamName(a.cParams[0])
		if pgIsRecordType(inner) {
			goVal = pgUnsafeNewExpr(inner, "unsafe.Pointer("+nm+")")
		} else { // writable scalar ref/ptr -> *GoFixed aliasing the C storage in place
			goVal = "(" + a.goType + ")(unsafe.Pointer(" + nm + "))"
		}
	case floatArg != "": // float bits -> the Go float value
		if len(a.cParams) != 1 {
			return nil, nil, nil, "", false
		}
		nm := cParamName(a.cParams[0])
		bitsExpr := "math.Float64frombits(uint64(" + nm + "))"
		if floatArg == "float32" {
			bitsExpr = "math.Float32frombits(uint32(" + nm + "))"
		}
		if a.goType == floatArg {
			goVal = bitsExpr
		} else { // typed alias over a float — rare, but convert cleanly
			goVal = a.goType + "(" + bitsExpr + ")"
		}
	default: // scalar / bool / enum: exactly one C param
		if len(a.cParams) != 1 || regTypes[0] == "uintptr" {
			return nil, nil, nil, "", false
		}
		goVal = pgScalarConv(a.goType, regTypes[0], cParamName(a.cParams[0]))
	}
	return cTypes, regDecls, regTypes, goVal, true
}

// pgFloatBits wraps a C++ float-typed dispatch argument in a bit-cast to its same-width unsigned
// integer, matching pgArgMarshal's bits-in-integer-register convention. Non-float args pass through.
func pgFloatBits(cParam, cppArg string) string {
	switch pgRegType(cTypeOf(cParam)) {
	case "float64":
		return "__builtin_bit_cast(unsigned long long, (double)(" + cppArg + "))"
	case "float32":
		return "__builtin_bit_cast(unsigned int, (float)(" + cppArg + "))"
	}
	return cppArg
}

// pgSigMarshal collects the marshalling for a signal's exposed args.
func pgSigMarshal(s signal) (cTypes, wrapperDecls, regTypes, vals []string, ok bool) {
	exposed := s.exposed
	if exposed > len(s.args) {
		exposed = len(s.args)
	}
	for _, a := range s.args[:exposed] {
		ct, rd, rt, v, k := pgArgMarshal(a)
		if !k {
			return nil, nil, nil, nil, false
		}
		cTypes = append(cTypes, ct...)
		wrapperDecls = append(wrapperDecls, rd...)
		regTypes = append(regTypes, rt...)
		vals = append(vals, v)
	}
	if len(regTypes)+1 > 15 { // purego NewCallback ceiling (id + args); maxArgs=15
		return nil, nil, nil, nil, false
	}
	return cTypes, wrapperDecls, regTypes, vals, true
}

// pgVirtRet classifies a virtual's return for the purego path. kind is one of:
//
//	"void"   — no return
//	"scalar" — scalar/bool/enum: returned directly (convFmt builds the register value)
//	"ptr"    — record-by-value / pointer-to-record: returned as the handle
//	"ptrlen" — QString or sequence container: returned as a malloc'd buffer + out-length
func pgVirtRet(v vmethod) (kind, retC, retReg, convFmt string, ok bool) {
	switch {
	case v.isVoid:
		return "void", "void", "", "", true
	case v.retQString:
		return "ptrlen", "char*", "uintptr", "", true
	case v.retList:
		if !v.lstr && !v.lel.ok {
			return "", "", "", "", false
		}
		if !v.lstr && !v.lel.isRecord && pgRegType(v.lel.cArrC) == "" {
			return "", "", "", "", false
		}
		return "ptrlen", "void*", "uintptr", "", true
	case v.cType == "void*": // record by-value / pointer-to-record: return the handle
		return "ptr", "void*", "uintptr", "uintptr(%s.UnsafePointer())", true
	default: // scalar / bool / enum / float
		reg := pgRegType(v.cType)
		if reg == "" || reg == "uintptr" || v.goRet == "" {
			return "", "", "", "", false
		}
		if reg == "float64" || reg == "float32" {
			// purego's NewCallback can't RETURN a float on SysV — carry the IEEE bits in an integer
			// register (dispatch returns uintptr; the C++ override memcpy's the bits back to the float).
			// convFmt carries the float width.
			return "scalarf", v.cType, "uintptr", reg, true
		}
		switch {
		case v.goRet == "bool":
			return "scalar", v.cType, reg, "b2i(%s)", true
		case v.goRet == reg:
			return "scalar", v.cType, reg, "%s", true
		default:
			return "scalar", v.cType, reg, reg + "(%s)", true
		}
	}
}

// pgVirtMarshal collects the marshalling for a virtual (args + return).
func pgVirtMarshal(v vmethod) (cTypes, wrapperDecls, regTypes, vals []string, kind, retC, retReg, convFmt string, ok bool) {
	for _, a := range v.args {
		ct, rd, rt, val, k := pgArgMarshal(a)
		if !k {
			return nil, nil, nil, nil, "", "", "", "", false
		}
		cTypes = append(cTypes, ct...)
		wrapperDecls = append(wrapperDecls, rd...)
		regTypes = append(regTypes, rt...)
		vals = append(vals, val)
	}
	kind, retC, retReg, convFmt, ok = pgVirtRet(v)
	if !ok {
		return nil, nil, nil, nil, "", "", "", "", false
	}
	overhead := 3 // goid + slot + args + handled
	if kind == "ptrlen" {
		overhead = 4 // + out-length
	}
	if len(regTypes)+overhead > 15 { // purego NewCallback ceiling; maxArgs=15
		return nil, nil, nil, nil, "", "", "", "", false
	}
	return cTypes, wrapperDecls, regTypes, vals, kind, retC, retReg, convFmt, true
}

func pgSigSupported(s signal) bool {
	_, _, _, _, ok := pgSigMarshal(s)
	return ok
}

func pgVirtSupported(v vmethod) bool {
	_, _, _, _, _, _, _, _, ok := pgVirtMarshal(v)
	return ok
}

// dispatchKind maps a return kind to the cbShape dispatch kind: "virtp" carries an out-length
// (QString/list); everything else is a single-value "virt".
func dispatchKind(retKind string) string {
	if retKind == "ptrlen" {
		return "virtp"
	}
	return "virt"
}

// pgVirtWrapper renders the Go wrapper closure stored in the virt registry: it adapts the generic
// dispatch's raw register args to the user's typed fn and converts the result back to the register
// representation the dispatch returns (a single value, or a (buffer, length) pair for QString/list).
func pgVirtWrapper(v vmethod, wrapperDecls, vals []string, kind, retReg, convFmt string) string {
	decls := strings.Join(wrapperDecls, ", ")
	call := "fn(" + strings.Join(vals, ", ") + ")"
	switch kind {
	case "void":
		return "func(" + decls + ") { " + call + " }"
	case "scalar", "ptr":
		return "func(" + decls + ") " + retReg + " { return " + fmt.Sprintf(convFmt, call) + " }"
	case "scalarf": // float return: hand the dispatch the IEEE bits in a uintptr (convFmt = float width)
		bits := "Float64bits"
		if convFmt == "float32" {
			bits = "Float32bits"
		}
		return "func(" + decls + ") uintptr { return uintptr(math." + bits + "(" + call + ")) }"
	case "ptrlen":
		var b strings.Builder
		fmt.Fprintf(&b, "func(%s) (uintptr, int64) {\n", decls)
		if v.retQString {
			fmt.Fprintf(&b, "\t\t__s := %s\n\t\treturn pgCString(__s), int64(len(__s))\n\t}", call)
			return b.String()
		}
		// sequence container -> a malloc'd C array (freed C++-side after rebuild) + count.
		var elemReg, elemSize, elemSet string
		switch {
		case v.lstr:
			elemReg, elemSize, elemSet = "uintptr", "unsafe.Sizeof(uintptr(0))", "pgCString(__e)"
		case v.lel.isRecord:
			elemReg, elemSize, elemSet = "uintptr", "unsafe.Sizeof(uintptr(0))", "uintptr(__e.UnsafePointer())"
		default:
			elemReg = pgRegType(v.lel.cArrC)
			elemSize = "unsafe.Sizeof(" + elemReg + "(0))"
			elemSet = elemReg + "(__e)"
		}
		fmt.Fprintf(&b, "\t\t__rs := %s\n\t\tif len(__rs) == 0 {\n\t\t\treturn 0, 0\n\t\t}\n", call)
		fmt.Fprintf(&b, "\t\t__a := pgMalloc(uintptr(len(__rs)) * %s)\n", elemSize)
		fmt.Fprintf(&b, "\t\t__sl := unsafe.Slice((*%s)(unsafe.Pointer(__a)), len(__rs))\n", elemReg)
		fmt.Fprintf(&b, "\t\tfor __i, __e := range __rs {\n\t\t\t__sl[__i] = %s\n\t\t}\n", elemSet)
		b.WriteString("\t\treturn __a, int64(len(__rs))\n\t}")
		return b.String()
	}
	return ""
}

// cbTok builds the dispatcher token + registers the shape. cTypes are the marshalled arg C
// types (between the fixed prefix and the handled flag).
func cbTok(kind, retC string, cTypes, regTypes []string, retReg string) string {
	tok := kind + "_" + shimShapeTok(retC)
	for _, c := range cTypes {
		tok += "_" + shimShapeTok(c)
	}
	if probing {
		return tok // Fixpoint renderability probe: don't register shapes for maybe-pruned records
	}
	cbShapes[tok] = cbShape{kind: kind, argRegs: regTypes, retReg: retReg}
	return tok
}

// pgConnectShimCpp renders the purego C++ connect shim for a signal: identical to the cgo
// lambda except the call target is the per-shape dispatch pointer (set from Go), not a
// //export. Returns "" if the shape is unsupported (the Go side skips it in lockstep).
func pgConnectShimCpp(cn, cls string, s signal) string {
	cTypes, _, regTypes, _, ok := pgSigMarshal(s)
	if !ok {
		return ""
	}
	tok := cbTok("sig", "void", cTypes, regTypes, "")
	var lambdaParams, argTypes, pre, callArgs []string
	callArgs = append(callArgs, "__id")
	for i, a := range s.args {
		lambdaParams = append(lambdaParams, a.cppType+" "+a.name)
		argTypes = append(argTypes, a.cppType)
		if i < s.exposed {
			pre = append(pre, a.cppPre...)
			for j, ca := range a.cppArgs { // float args cross as bits (see pgArgMarshal)
				if j < len(a.cParams) {
					ca = pgFloatBits(a.cParams[j], ca)
				}
				callArgs = append(callArgs, ca)
			}
		}
	}
	memberPtr := fmt.Sprintf("&%s::%s", cls, s.member)
	if s.overloaded {
		memberPtr = fmt.Sprintf("static_cast<void(%s::*)(%s)>(&%s::%s)", cls, strings.Join(argTypes, ", "), cls, s.member)
	}
	fnType := "void(*)(long long" + cTypesPrefixed(cTypes) + ")"
	var b strings.Builder
	fmt.Fprintf(&b, "void %s(void* self, long long __id) {\n", s.connectC)
	fmt.Fprintf(&b, "\tQMetaObject::Connection __c = QObject::connect((%s*)self, %s, (%s*)self, [__id](%s){\n",
		cls, memberPtr, cls, strings.Join(lambdaParams, ", "))
	for _, p := range pre {
		fmt.Fprintf(&b, "\t\t%s\n", p)
	}
	fmt.Fprintf(&b, "\t\tif (cppgo_%s) ((%s)cppgo_%s)(%s);\n\t});\n", tok, fnType, tok, strings.Join(callArgs, ", "))
	fmt.Fprintf(&b, "\tstd::lock_guard<std::mutex> __lk(__connsMu_%s);\n\t__conns_%s[__id] = __c;\n}\n", cn, cn)
	return b.String()
}

// pgVirtOverrideCpp renders the purego C++ override for a virtual (inside the __Go subclass):
// identical to the cgo override except it calls the per-shape dispatch pointer with the goid +
// slot-key literal instead of a //export trampoline. Returns "" if the shape is unsupported.
func pgVirtOverrideCpp(cls string, v vmethod) string {
	cTypes, _, regTypes, _, kind, retC, retReg, convFmt, ok := pgVirtMarshal(v)
	if !ok {
		// purego can't dispatch this shape — it exceeds purego's NewCallback arg ceiling (a hard limit,
		// not a deferral; every marshallable shape IS dispatched). A regular virtual just inherits the base
		// (emit nothing). A PURE virtual MUST be overridden or the __Go subclass is abstract and the class
		// is uninstantiable from Go — so emit a default-returning override: the class stays constructible,
		// but this one (rare, >9-arg) virtual can't be overridden cgo-free. The Go side skips its On<X>.
		if !v.isPure {
			return ""
		}
		cst := ""
		if v.isConst {
			cst = " const"
		}
		var lp []string
		for _, a := range v.args {
			lp = append(lp, a.cppType+" "+a.name)
		}
		if v.isVoid {
			return fmt.Sprintf("\t%s %s(%s)%s override {}\n", v.cppRet, v.member, strings.Join(lp, ", "), cst)
		}
		dv := v.cppRet + "{}"
		switch {
		case strings.HasSuffix(v.cppRet, "*"):
			dv = "nullptr"
		case strings.Contains(v.cppRet, " "):
			dv = "(" + v.cppRet + ")0"
		}
		return fmt.Sprintf("\t%s %s(%s)%s override { return %s; }\n", v.cppRet, v.member, strings.Join(lp, ", "), cst, dv)
	}
	cst := ""
	if v.isConst {
		cst = " const"
	}
	var lambdaParams, trampArgs, baseArgs, pre []string
	for _, a := range v.args {
		lambdaParams = append(lambdaParams, a.cppType+" "+a.name)
		pre = append(pre, a.cppPre...)
		for j, ca := range a.cppArgs { // float args cross as bits (see pgArgMarshal)
			if j < len(a.cParams) {
				ca = pgFloatBits(a.cParams[j], ca)
			}
			trampArgs = append(trampArgs, ca)
		}
		baseArgs = append(baseArgs, a.name)
	}
	// fallthrough (not-handled): a regular virtual calls the base; a pure virtual returns a default.
	fallVoid := fmt.Sprintf("%s::%s(%s)", cls, v.member, strings.Join(baseArgs, ", "))
	fallVal := fallVoid
	if v.isPure {
		fallVoid = "return"
		switch {
		case strings.HasSuffix(v.cppRet, "*"):
			fallVal = "nullptr"
		case strings.Contains(v.cppRet, " "):
			fallVal = "(" + v.cppRet + ")0"
		default:
			fallVal = v.cppRet + "{}"
		}
	}
	tok := cbTok(dispatchKind(kind), retC, cTypes, regTypes, retReg)
	// The slot crosses as a generator-computed int64 hash (no string on the hot path), and the whole
	// crossing is gated on the instance's override mask — an unregistered virtual never leaves C++.
	slot := fmt.Sprintf("%dLL", v.slotID)
	gate := fmt.Sprintf("__goOn[%d] && ", v.idx)
	var b strings.Builder
	fmt.Fprintf(&b, "\t%s %s(%s)%s override {\n\t\tint __h = 0;\n", v.cppRet, v.member, strings.Join(lambdaParams, ", "), cst)
	for _, p := range pre {
		fmt.Fprintf(&b, "\t\t%s\n", p)
	}
	switch kind {
	case "void":
		fnType := "void(*)(long long, long long" + cTypesPrefixed(cTypes) + ", int*)"
		call := fmt.Sprintf("((%s)cppgo_%s)(__goId, %s%s, &__h)", fnType, tok, slot, cppArgsPrefixed(trampArgs))
		fmt.Fprintf(&b, "\t\tif (%scppgo_%s) { %s; if (__h) return; }\n\t\t%s;\n\t}\n", gate, tok, call, fallVoid)
	case "scalar", "ptr":
		fnType := retC + "(*)(long long, long long" + cTypesPrefixed(cTypes) + ", int*)"
		call := fmt.Sprintf("((%s)cppgo_%s)(__goId, %s%s, &__h)", fnType, tok, slot, cppArgsPrefixed(trampArgs))
		fmt.Fprintf(&b, "\t\tif (%scppgo_%s) { %s __r = %s; if (__h) return %s; }\n\t\treturn %s;\n\t}\n",
			gate, tok, v.cType, call, fmt.Sprintf(v.cppCast, "__r"), fallVal)
	case "scalarf": // float return carried as integer bits (dispatch returns uintptr; memcpy back)
		bitsC := "unsigned long long"
		if convFmt == "float32" {
			bitsC = "unsigned int"
		}
		fnType := bitsC + "(*)(long long, long long" + cTypesPrefixed(cTypes) + ", int*)"
		call := fmt.Sprintf("((%s)cppgo_%s)(__goId, %s%s, &__h)", fnType, tok, slot, cppArgsPrefixed(trampArgs))
		fmt.Fprintf(&b, "\t\tif (%scppgo_%s) { %s __b = %s; if (__h) { %s __r; __builtin_memcpy(&__r, &__b, sizeof(__r)); return __r; } }\n\t\treturn %s;\n\t}\n",
			gate, tok, bitsC, call, v.cType, fallVal)
	case "ptrlen":
		fnType := retC + "(*)(long long, long long" + cTypesPrefixed(cTypes) + ", long long*, int*)"
		call := fmt.Sprintf("((%s)cppgo_%s)(__goId, %s%s, &__len, &__h)", fnType, tok, slot, cppArgsPrefixed(trampArgs))
		if v.retQString {
			fmt.Fprintf(&b, "\t\tif (%scppgo_%s) {\n\t\t\tlong long __len = 0;\n\t\t\tchar* __s = %s;\n\t\t\tif (__h) { QString __r = QString::fromUtf8(__s, __len); free(__s); return __r; }\n\t\t}\n\t\treturn %s;\n\t}\n",
				gate, tok, call, fallVal)
		} else { // sequence container
			fmt.Fprintf(&b, "\t\tif (%scppgo_%s) {\n\t\t\tlong long __len = 0;\n\t\t\tvoid* __a = %s;\n\t\t\tif (__h) {\n\t\t\t\t%s __r; __r.reserve(__len);\n", gate, tok, call, v.cppRet)
			if v.lstr {
				fmt.Fprintf(&b, "\t\t\t\tchar** __aa = (char**)__a;\n\t\t\t\tfor (long long __i = 0; __i < __len; __i++) { __r.%s(QString::fromUtf8(__aa[__i])); free(__aa[__i]); }\n", v.ladd)
			} else {
				arrCType := v.lel.cArrC
				if v.lel.isRecord {
					arrCType = "void*"
				}
				fmt.Fprintf(&b, "\t\t\t\t%s* __aa = (%s*)__a;\n\t\t\t\tfor (long long __i = 0; __i < __len; __i++) __r.%s(%s);\n",
					arrCType, arrCType, v.ladd, fmt.Sprintf(v.lel.cppFrom, "__aa[__i]"))
			}
			fmt.Fprintf(&b, "\t\t\t\tif (__a) free(__a);\n\t\t\t\treturn __r;\n\t\t\t}\n\t\t}\n\t\treturn %s;\n\t}\n", fallVal)
		}
	}
	return b.String()
}

// cppArgsPrefixed renders ", a0, a1, ..." (leading comma) or "" for the C++ dispatch call args.
func cppArgsPrefixed(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return ", " + strings.Join(args, ", ")
}

// cTypesPrefixed renders ", t0, t1, ..." (leading comma) or "" for an empty list.
func cTypesPrefixed(cTypes []string) string {
	if len(cTypes) == 0 {
		return ""
	}
	return ", " + strings.Join(cTypes, ", ")
}

// EmittedCbHeader renders gen_purego_cb.h: extern decls for every dispatch pointer + setter.
func EmittedCbHeader() string {
	if len(cbShapes) == 0 {
		return ""
	}
	toks := cbToksSorted()
	var b strings.Builder
	b.WriteString("#pragma once\n// Code generated by cppgo; DO NOT EDIT. purego C->Go dispatch pointers (cgo-free backend).\n")
	b.WriteString("#ifdef __cplusplus\nextern \"C\" {\n#endif\n")
	for _, t := range toks {
		fmt.Fprintf(&b, "extern void* cppgo_%s;\nvoid cppgo_set_%s(void* fn);\n", t, t)
	}
	b.WriteString("#ifdef __cplusplus\n}\n#endif\n")
	return b.String()
}

// EmittedCbCpp renders gen_purego_cb.cpp: the dispatch pointer definitions + setters.
func EmittedCbCpp() string {
	if len(cbShapes) == 0 {
		return ""
	}
	toks := cbToksSorted()
	var b strings.Builder
	b.WriteString("// Code generated by cppgo; DO NOT EDIT. purego C->Go dispatch pointers (cgo-free backend).\n")
	b.WriteString("#include \"gen_purego_cb.h\"\n\nextern \"C\" {\n")
	for _, t := range toks {
		fmt.Fprintf(&b, "void* cppgo_%s = 0;\nvoid cppgo_set_%s(void* fn) { cppgo_%s = fn; }\n", t, t, t)
	}
	b.WriteString("}\n")
	return b.String()
}

// EmittedCbGo renders gen_purego_cb.go: one generic dispatch func per shape + the init that
// binds the setters and installs the purego.NewCallback for each.
func EmittedCbGo(pkg string) string {
	if len(cbShapes) == 0 {
		return ""
	}
	toks := cbToksSorted()
	var dispatch, vars, inits strings.Builder
	// EVERY dispatch function returns exactly one uintptr: Windows' syscall.NewCallback (which
	// purego delegates to there) requires one uintptr-sized result — a void or int32-returning
	// callback panics at init. On unix the extra/wider register is simply ignored by the C++
	// caller's (correctly narrower-typed) function pointer, so one shape serves every platform.
	for _, t := range toks {
		s := cbShapes[t]
		var ps, names []string
		for i, r := range s.argRegs {
			ps = append(ps, fmt.Sprintf("a%d %s", i, r))
			names = append(names, fmt.Sprintf("a%d", i))
		}
		funcType := "func(" + strings.Join(s.argRegs, ", ") + ")"
		switch s.kind {
		case "sig":
			fmt.Fprintf(&dispatch, "func _%s(__id int64%s) uintptr {\n\tif w, ok := lookupCallback(__id).(%s); ok {\n\t\tw(%s)\n\t}\n\treturn 0\n}\n\n",
				t, prefixDecls(ps), funcType, strings.Join(names, ", "))
		case "virtp": // QString/list return: the wrapper returns (buffer, length); dispatch sets *outlen.
			fmt.Fprintf(&dispatch, "func _%s(__goid int64, __slot int64%s, __outlen uintptr, __handled uintptr) uintptr {\n", t, prefixDecls(ps))
			fmt.Fprintf(&dispatch, "\tif w, ok := lookupVirt(__goid, __slot).(%s (uintptr, int64)); ok {\n", funcType)
			fmt.Fprintf(&dispatch, "\t\t__p, __n := w(%s)\n\t\t*(*int64)(unsafe.Pointer(__outlen)) = __n\n\t\t*(*int32)(unsafe.Pointer(__handled)) = 1\n\t\treturn __p\n\t}\n", strings.Join(names, ", "))
			dispatch.WriteString("\t*(*int32)(unsafe.Pointer(__handled)) = 0\n\treturn 0\n}\n\n")
		default: // "virt": single-value (void / scalar / handle) return, carried in the uintptr
			if s.retReg != "" {
				funcType += " " + s.retReg
			}
			fmt.Fprintf(&dispatch, "func _%s(__goid int64, __slot int64%s, __handled uintptr) uintptr {\n\tif w, ok := lookupVirt(__goid, __slot).(%s); ok {\n\t\t*(*int32)(unsafe.Pointer(__handled)) = 1\n",
				t, prefixDecls(ps), funcType)
			if s.retReg == "" {
				fmt.Fprintf(&dispatch, "\t\tw(%s)\n\t\treturn 0\n\t}\n\t*(*int32)(unsafe.Pointer(__handled)) = 0\n\treturn 0\n}\n\n", strings.Join(names, ", "))
			} else {
				fmt.Fprintf(&dispatch, "\t\treturn %s\n\t}\n\t*(*int32)(unsafe.Pointer(__handled)) = 0\n\treturn 0\n}\n\n",
					cbRetToUintptr(s.retReg, "w("+strings.Join(names, ", ")+")"))
			}
		}
		fmt.Fprintf(&vars, "\tpg_set_%s func(uintptr)\n", t)
		fmt.Fprintf(&inits, "\treg(&pg_set_%s, \"cppgo_set_%s\")\n\tpg_set_%s(purego.NewCallback(_%s))\n", t, t, t, t)
	}
	hasVirt := false
	for _, t := range toks {
		if cbShapes[t].kind == "virt" || cbShapes[t].kind == "virtp" {
			hasVirt = true // virt dispatch writes the `handled` flag through unsafe.Pointer
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "package %s\n\n", pkg)
	b.WriteString("// Code generated by cppgo; DO NOT EDIT. purego C->Go dispatch (cgo-free backend).\n\n")
	b.WriteString("import (\n")
	if hasVirt {
		b.WriteString("\t\"unsafe\"\n\n")
	}
	b.WriteString("\t\"github.com/ebitengine/purego\"\n)\n\n")
	fmt.Fprintf(&b, "var (\n%s)\n\n", vars.String())
	fmt.Fprintf(&b, "func init() {\n\t_load()\n%s}\n\n", inits.String())
	b.WriteString(dispatch.String())
	return b.String()
}

func prefixDecls(ps []string) string {
	if len(ps) == 0 {
		return ""
	}
	return ", " + strings.Join(ps, ", ")
}

func cbToksSorted() []string {
	toks := make([]string, 0, len(cbShapes))
	for t := range cbShapes {
		toks = append(toks, t)
	}
	sort.Strings(toks)
	return toks
}
