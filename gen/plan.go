package gen

import (
	"fmt"
	"strings"

	"github.com/5k3105/cppgo/ir"
)

// ---- scalar/record classification (single-value types) ----

type scalarPlan struct {
	ok           bool
	cType        string
	goType       string
	handle       bool   // a bound-record handle (*T wrapper): the CALLER must KeepAlive the wrapper across the shim call, or its GC finalizer could free the C++ object mid-call
	cgoArg       string // fmt: build cgo arg from Go param name
	cppParamCast string // fmt: C++ expr from the C param name
	cppRetWrap   string // fmt: C++ return expr from the captured call result
	goRetWrap    string // fmt: Go return expr from the captured cgo result
}

// isQString reports whether t (after stripping const/ref/pointer) is a QString record.
// QString crosses the ABI as UTF-8 char*+len, not as a void* handle, so it is detected
// here rather than going through the record path in classifyScalar.
func isQString(t ir.Type) bool {
	switch t.Kind {
	case "record":
		return t.Name == "QString"
	case "ref":
		// Only a CONST QString& is a read-only string input. A non-const QString& is a mutable
		// in/out parameter (QValidator::fixup(QString&) rewrites it in place) — we can't marshal that
		// as a by-value Go string, so leave it unmatched -> the method/override is dropped, not
		// mis-bound with an added const (which also makes a virtual override fail to match its base).
		return t.Const && t.Elem != nil && t.Elem.Name == "QString"
	// NOT "pointer": a QString* is likewise a mutable / out parameter (QDebug(QString*), moveToTrash's
	// out path) we can't marshal as a by-value Go string; unmatched -> the method is dropped.
	case "typedef":
		return t.Canonical != nil && isQString(*t.Canonical)
	}
	return false
}

// isQAnyStringView reports whether t (after const/ref) is QAnyStringView. Many Qt6 setters now take
// it instead of QString; it crosses as UTF-8 char*+len like QString but builds a (non-owning) view —
// safe because the callee copies it during the call.
func isQAnyStringView(t ir.Type) bool {
	switch t.Kind {
	case "record":
		return t.Name == "QAnyStringView"
	case "ref", "pointer":
		return t.Elem != nil && t.Elem.Name == "QAnyStringView"
	case "typedef":
		return t.Canonical != nil && isQAnyStringView(*t.Canonical)
	}
	return false
}

// isConstCharPtr reports whether t is a `const char*` (a C string) — bound to a Go string. A non-const
// char* is an in/out buffer we don't marshal that way. Resolves through typedefs.
func isConstCharPtr(t ir.Type) bool {
	if t.Kind == "typedef" && t.Canonical != nil {
		return isConstCharPtr(*t.Canonical)
	}
	return t.Kind == "pointer" && t.Const && t.Elem != nil && t.Elem.Kind == "builtin" &&
		(t.Elem.Name == "char" || t.Elem.Name == "signed char")
}

func classifyScalar(t ir.Type, rv Resolver) scalarPlan {
	switch t.Kind {
	case "builtin":
		c, g, ok := builtinMap(t.Name)
		if !ok {
			return scalarPlan{}
		}
		p := scalarPlan{ok: true, cType: c, goType: g, cppParamCast: "%s", cppRetWrap: "%s"}
		if g == "bool" {
			p.cgoArg, p.goRetWrap = "b2i(%s)", "%s != 0"
		} else {
			p.cgoArg, p.goRetWrap = cgoName(c)+"(%s)", g+"(%s)"
		}
		return p
	case "enum":
		goT := "int" // enums cross the ABI as int...
		if en, ok := rv.EnumName[t.USR]; ok {
			// ...with a typed Go alias (miqt-style flat/Class__ name), package-qualified cross-package.
			goT = rv.prefix(rv.EnumPkg[t.USR]) + GoEnumType(en)
		}
		// An enum with enumerators outside 32 bits (QAnyStringView::Tag uses the top two bits of a
		// 64-bit underlying type) would TRUNCATE through the default C `int` crossing — carry those
		// as long long instead. Detected per-enum in cmd/gen from the constant values (rv.Enum64).
		if rv.Enum64[t.USR] {
			return scalarPlan{ok: true, cType: "long long", goType: goT,
				cgoArg: "C.longlong(%s)", cppParamCast: "(" + t.Name + ")(%s)", cppRetWrap: "(long long)(%s)", goRetWrap: goT + "(%s)"}
		}
		return scalarPlan{ok: true, cType: "int", goType: goT,
			cgoArg: "C.int(%s)", cppParamCast: "(" + t.Name + ")(%s)", cppRetWrap: "(int)(%s)", goRetWrap: goT + "(%s)"}
	case "typedef":
		// std::chrono::duration (nanoseconds/milliseconds/seconds/...) <-> Go int64 (the count in the
		// alias's own unit). Param: construct the duration from the count; return: .count(). Qt6 timer/
		// animation APIs (startTimer, QTimer, QAbstractAnimation) take these.
		if strings.HasPrefix(t.Alias, "std::chrono::") && t.Canonical != nil &&
			t.Canonical.Kind == "template" && t.Canonical.Head == "duration" {
			return scalarPlan{ok: true, cType: "long long", goType: "int64",
				cgoArg: "C.longlong(%s)", cppParamCast: t.Alias + "((long long)%s)",
				cppRetWrap: "(long long)(%s).count()", goRetWrap: "int64(%s)"}
		}
		// quintptr/qintptr are pointer-sized integers (the opaque id stored in a QModelIndex via
		// createIndex(row,col,quintptr), QWidget::winId() -> WId, etc.). Map to Go `uintptr` for miqt
		// parity instead of the canonical uint64/int64 — removes a uint64() cast at every model
		// CreateIndex/InternalId site. ABI still crosses as the canonical 8-byte integer.
		if (t.Alias == "quintptr" || t.Alias == "qintptr") && t.Canonical != nil {
			if c, _, ok := builtinMap(t.Canonical.Name); ok {
				return scalarPlan{ok: true, cType: c, goType: "uintptr",
					cgoArg: cgoName(c) + "(%s)", cppParamCast: "%s", cppRetWrap: "%s", goRetWrap: "uintptr(%s)"}
			}
		}
		if strings.HasPrefix(t.Alias, "std::") {
			return scalarPlan{} // std:: types aren't bound (and libclang often degrades them to int)
		}
		if t.Canonical != nil {
			return classifyScalar(*t.Canonical, rv)
		}
	case "template":
		if t.Head == "QFlags" {
			// QFlags<Enum> binds as the Enum's typed Go alias (like miqt), so the typed flag CONSTANTS
			// (qt.AlignCenter, ...) pass directly to params and combine on returns (m.Modifiers() &
			// qt.ShiftModifier) without an int() cast. Crosses the C ABI as int.
			goT := "int"
			cast := "%s"
			if len(t.Args) == 1 && t.Args[0].Name != "" {
				cast = "QFlags<" + t.Args[0].Name + ">(%s)"
				if t.Args[0].Kind == "enum" {
					if en, ok := rv.EnumName[t.Args[0].USR]; ok {
						goT = rv.prefix(rv.EnumPkg[t.Args[0].USR]) + GoEnumType(en)
					}
				}
			}
			return scalarPlan{ok: true, cType: "int", goType: goT,
				cgoArg: "C.int(%s)", cppParamCast: cast, cppRetWrap: "(int)(%s)", goRetWrap: goT + "(%s)"}
		}
	case "record":
		// A by-value record bind copies it (param: `*(Cls*)x`; return: `new Cls(__r)`), so the type
		// must be copyable. Move-only types (QStringConverter) fall through here -> method dropped;
		// they remain bindable by ref/pointer (borrowed handle, no copy) below.
		if gt, bare, cpp, pfx, ok := rv.rec(t.USR); ok && rv.Copyable[t.USR] {
			return scalarPlan{ok: true, cType: "void*", goType: "*" + gt, handle: true, cgoArg: "%s.UnsafePointer()",
				cppParamCast: "*(" + cpp + "*)%s", cppRetWrap: "new " + cpp + "(%s)", goRetWrap: pfx + "UnsafeNew" + bare + "Owned(%s)"}
		}
	case "ref":
		if t.Elem != nil && t.Elem.Kind == "record" {
			if gt, bare, cpp, pfx, ok := rv.rec(t.Elem.USR); ok {
				return scalarPlan{ok: true, cType: "void*", goType: "*" + gt, handle: true, cgoArg: "%s.UnsafePointer()",
					cppParamCast: "*(" + cpp + "*)%s", cppRetWrap: "(void*)&(%s)", goRetWrap: pfx + "UnsafeNew" + bare + "(%s)"}
			}
		}
	case "pointer":
		if t.Elem != nil && t.Elem.Kind == "record" {
			if gt, bare, cpp, pfx, ok := rv.rec(t.Elem.USR); ok {
				return scalarPlan{ok: true, cType: "void*", goType: "*" + gt, handle: true, cgoArg: "%s.UnsafePointer()",
					cppParamCast: "(" + cpp + "*)%s", cppRetWrap: "(void*)(%s)", goRetWrap: pfx + "UnsafeNew" + bare + "(%s)"}
			}
		}
	}
	return scalarPlan{}
}

// cTypeGoFixed maps a C ABI scalar type to the fixed-width Go type that aliases it byte-for-byte (so a
// *T pointer can be cast straight onto C memory). Returns "" for types whose width is platform-ambiguous
// or unknown — the caller then declines the in/out binding. Used for mutable-reference (in/out) scalars.
func cTypeGoFixed(cType string) string {
	switch cType {
	case "int":
		return "int32"
	case "unsigned int":
		return "uint32"
	case "long long":
		return "int64"
	case "unsigned long long":
		return "uint64"
	case "short":
		return "int16"
	case "unsigned short":
		return "uint16"
	case "double":
		return "float64"
	case "float":
		return "float32"
	}
	return ""
}

// listElem describes the element type of a QList<...>.
type listElem struct {
	ok       bool
	isRecord bool
	strElem  bool   // element is QString, crossing as a malloc'd char* (needs goFree after a param call; only the PARAM direction is supported via classifyListElem — seq RETURNs of QString have their own path)
	cppElem  string // C++ container element type ("QByteArray", "int", or an enum like "QFontDatabase::WritingSystem")
	cArrC    string // C ABI array element type (== cppElem for builtins; "int" for enums; unused for records, which use void**)
	goElem   string // Go element type ("*QByteArray" or "int64")
	// per-element marshalling (one %s = the element expression):
	cppNew  string // C++: heap-copy a returned element ("new QByteArray(%s)" | "%s")
	goWrap  string // Go: wrap a returned element handle/value
	cppFrom string // C++: build an element for a param list from the C array item
	goToC   string // Go: convert a Go element to the cgo array item
	goFree  string // Go: free a built array item after a param call (strElem only; "" otherwise)
	cArrGo  string // Go cgo element type for the returned array ("unsafe.Pointer" | "C.longlong")
}

func classifyListElem(t ir.Type, rv Resolver) listElem {
	if isQString(t) { // QString element -> Go string (char* on the wire); param-direction only (free after).
		return listElem{ok: true, strElem: true, cppElem: "QString", cArrC: "char*", goElem: "string",
			cppFrom: "QString::fromUtf8(%s)", goToC: "C.CString(%s)", goFree: "C.free(unsafe.Pointer(%s))",
			cArrGo: "*C.char", goWrap: "C.GoString(%s)", cppNew: "%s"}
	}
	switch t.Kind {
	case "record":
		if gt, bare, cpp, pfx, ok := rv.rec(t.USR); ok {
			return listElem{ok: true, isRecord: true, cppElem: cpp, cArrC: "void*", goElem: "*" + gt,
				cppNew: "new " + cpp + "(%s)", goWrap: pfx + "UnsafeNew" + bare + "Owned(%s)",
				cppFrom: "*(" + cpp + "*)%s", goToC: "%s.UnsafePointer()", cArrGo: "unsafe.Pointer"}
		}
	case "pointer":
		// QList<QFoo*> (QObjectList, QWidgetList, QList<QAction*>, ...) -> []*QFoo. Each element is a
		// BORROWED handle (Qt owns the pointee), so no copy and UnsafeNew (not ...Owned).
		if t.Elem != nil && t.Elem.Kind == "record" {
			if gt, bare, cpp, pfx, ok := rv.rec(t.Elem.USR); ok {
				return listElem{ok: true, isRecord: true, cppElem: cpp + "*", cArrC: "void*", goElem: "*" + gt,
					cppNew: "(void*)(%s)", goWrap: pfx + "UnsafeNew" + bare + "(%s)",
					cppFrom: "(" + cpp + "*)%s", goToC: "%s.UnsafePointer()", cArrGo: "unsafe.Pointer"}
			}
		}
	case "enum":
		// QList<SomeEnum> -> []SomeEnumGoType. The container element is the C++ enum type, but it crosses
		// the wire as int (cArrC), so cppNew casts enum->int and cppFrom casts int->enum. A 64-bit-
		// underlying enum would truncate through that int — drop rather than mis-bind (none in Qt today).
		if rv.Enum64[t.USR] {
			return listElem{}
		}
		goT := "int"
		if en, ok := rv.EnumName[t.USR]; ok {
			goT = rv.prefix(rv.EnumPkg[t.USR]) + GoEnumType(en)
		}
		return listElem{ok: true, cppElem: t.Name, cArrC: "int", goElem: goT,
			cppNew: "(int)(%s)", goWrap: goT + "(%s)", cppFrom: "(" + t.Name + ")%s", goToC: "C.int(%s)", cArrGo: "C.int"}
	case "builtin":
		if c, g, ok := builtinMap(t.Name); ok && g != "" {
			return listElem{ok: true, cppElem: c, cArrC: c, goElem: g,
				cppNew: "%s", goWrap: g + "(%s)", cppFrom: "%s", goToC: cgoName(c) + "(%s)", cArrGo: cgoName(c)}
		}
	case "typedef":
		if strings.HasPrefix(t.Alias, "std::") {
			return listElem{}
		}
		if t.Canonical != nil {
			return classifyListElem(*t.Canonical, rv)
		}
	}
	return listElem{}
}

// seqContainer maps a sequence-container template head to its C++ type and element-add
// method. All three marshal to/from a Go []T. (QVector is an alias of QList in Qt 6.)
func seqContainer(head string) (cppName, add string, ok bool) {
	switch head {
	case "QList":
		return "QList", "append", true
	case "QVector":
		return "QVector", "append", true
	case "QSet":
		return "QSet", "insert", true
	}
	return "", "", false
}

// assocContainer maps an associative-container head to its C++ type. Both marshal to/from
// a Go map[K]V (scalar/enum KEY only — a record key can't be a Go map key by value).
func assocContainer(head string) (cppName string, ok bool) {
	switch head {
	case "QMap":
		return "QMap", true
	case "QHash":
		return "QHash", true
	}
	return "", false
}

// isPair reports whether a template head is QPair / std::pair (the same type in Qt 6).
func isPair(head string) bool { return head == "QPair" || head == "pair" }

// ---- per-parameter / per-return plans (may emit multiple statements) ----

type paramPlan struct {
	cParams []string // C ABI param decls contributed
	goParam string   // Go param decl ("" for an OUTPUT param — it is a return value, not an input)
	goPre   []string // Go statements before the call
	goArgs  []string // cgo call arguments
	goPost  []string // Go statements AFTER the call (e.g. free C strings built for a []string param)
	// keepAlive: Go values that must stay reachable across the shim call — wrapper handles (whose GC
	// finalizer would otherwise be free to run mid-call and delete the C++ object) and Go buffers whose
	// raw pointer was extracted before the call. The emitters append runtime.KeepAlive(v) after the call.
	keepAlive []string
	cppArg    string   // C++ argument expression
	cppPre    []string // C++ statements before the call
	cppPost   []string // C++ statements after the call (e.g. write an output param back to its C-ABI pointer)
	// Output parameter (a non-const scalar pointer the method writes): surfaced as an extra Go return.
	outRetType string // Go type of the extra return value ("" = ordinary input param)
	outRetExpr string // Go expression producing that return value (reads the populated local)
}

func planParam(nm string, t ir.Type, rv Resolver) (paramPlan, bool) {
	// QString param -> Go string. Crosses as (const char*, len); the shim rebuilds via
	// QString::fromUtf8. The byte slice is Go-heap memory with no Go pointers, so passing
	// &b[0] to C is allowed (fromUtf8 copies immediately).
	if isQString(t) || isQAnyStringView(t) {
		build := fmt.Sprintf("QString %s_qstr = QString::fromUtf8(%s, %s_len);", nm, nm, nm)
		if isQAnyStringView(t) {
			build = fmt.Sprintf("QAnyStringView %s_qstr = QAnyStringView(%s, %s_len);", nm, nm, nm)
		}
		p := paramPlan{
			cParams: []string{"const char* " + nm, "long long " + nm + "_len"},
			goParam: nm + " string",
			cppArg:  nm + "_qstr",
		}
		// Pass the string's own bytes (no []byte copy): the shim copies immediately (fromUtf8 /
		// QAnyStringView consumed during the call), string data holds no Go pointers, and keepAlive
		// pins the string across the call. Saves one allocation per string argument.
		p.goPre = []string{
			fmt.Sprintf("var %s_p *C.char", nm),
			fmt.Sprintf("if len(%s) > 0 {", nm),
			fmt.Sprintf("\t%s_p = (*C.char)(unsafe.Pointer(unsafe.StringData(%s)))", nm, nm),
			"}",
		}
		p.goArgs = []string{nm + "_p", fmt.Sprintf("C.longlong(len(%s))", nm)}
		p.keepAlive = []string{nm}
		p.cppPre = []string{build}
		return p, true
	}

	// const char* param -> Go string. Crosses as a NUL-terminated C string built in Go memory (append a
	// 0 byte, no malloc), passed straight through (the callee copies/uses it during the call).
	if isConstCharPtr(t) {
		return paramPlan{
			cParams: []string{"const char* " + nm},
			goParam: nm + " string",
			goPre: []string{
				fmt.Sprintf("%s_b := append([]byte(%s), 0)", nm, nm),
				fmt.Sprintf("%s_p := (*C.char)(unsafe.Pointer(&%s_b[0]))", nm, nm),
			},
			goArgs: []string{nm + "_p"},
			cppArg: nm,
		}, true
	}

	// Unwrap a const-ref and typedefs to reach an underlying container template: `const QList<X>&` and
	// `const QStringList&` are the common Qt param forms (the by-value container marshalling then applies).
	ct := t
	for ct.Kind == "ref" && ct.Const && ct.Elem != nil {
		ct = *ct.Elem
	}
	for ct.Kind == "typedef" && ct.Canonical != nil {
		ct = *ct.Canonical
	}

	// sequence container of QString (QStringList / QList<QString>) param -> Go []string. Each element is
	// a malloc'd C string (freed after the call via goPost); the shim rebuilds the QList<QString>.
	if _, _, isSeq := seqContainer(ct.Head); ct.Kind == "template" && isSeq && len(ct.Args) == 1 && isQString(ct.Args[0]) {
		cont, add, _ := seqContainer(ct.Head)
		return paramPlan{
			cParams: []string{"char** " + nm, "long long " + nm + "_len"},
			goParam: nm + " []string",
			goPre: []string{
				fmt.Sprintf("%s_cs := make([]*C.char, len(%s))", nm, nm),
				fmt.Sprintf("for __i, __e := range %s {", nm),
				fmt.Sprintf("\t%s_cs[__i] = C.CString(__e)", nm),
				"}",
				fmt.Sprintf("var %s_p **C.char", nm),
				fmt.Sprintf("if len(%s_cs) > 0 {", nm),
				fmt.Sprintf("\t%s_p = &%s_cs[0]", nm, nm),
				"}",
			},
			goArgs: []string{nm + "_p", fmt.Sprintf("C.longlong(len(%s))", nm)},
			goPost: []string{
				fmt.Sprintf("for __i := range %s_cs {", nm),
				fmt.Sprintf("\tC.free(unsafe.Pointer(%s_cs[__i]))", nm),
				"}",
			},
			cppArg: nm + "_list",
			cppPre: []string{
				fmt.Sprintf("%s<QString> %s_list; %s_list.reserve(%s_len);", cont, nm, nm, nm),
				fmt.Sprintf("for (long long __i = 0; __i < %s_len; __i++) %s_list.%s(QString::fromUtf8(%s[__i]));", nm, nm, add, nm),
			},
		}, true
	}

	// sequence container (QList/QVector/QSet)<elem> param -> (void**|T*) + length
	if cont, add, isSeq := seqContainer(ct.Head); ct.Kind == "template" && isSeq && len(ct.Args) == 1 {
		t := ct // marshal the unwrapped container
		el := classifyListElem(t.Args[0], rv)
		if !el.ok {
			return paramPlan{}, false
		}
		arrC := el.cArrC + "*"
		if el.isRecord {
			arrC = "void**"
		}
		p := paramPlan{
			cParams: []string{arrC + " " + nm, "long long " + nm + "_len"},
			goParam: nm + " []" + el.goElem,
			cppArg:  nm + "_list",
		}
		p.goPre = []string{
			fmt.Sprintf("%s_arr := make([]%s, len(%s))", nm, el.cArrGo, nm),
			fmt.Sprintf("for __i, __e := range %s {", nm),
			fmt.Sprintf("\t%s_arr[__i] = %s", nm, fmt.Sprintf(el.goToC, "__e")),
			"}",
			fmt.Sprintf("var %s_ptr *%s", nm, el.cArrGo),
			fmt.Sprintf("if len(%s_arr) > 0 {", nm),
			fmt.Sprintf("\t%s_ptr = &%s_arr[0]", nm, nm),
			"}",
		}
		p.goArgs = []string{fmt.Sprintf("(%s)(unsafe.Pointer(%s_ptr))", cArrPtrGo(arrC), nm), fmt.Sprintf("C.longlong(len(%s))", nm)}
		if el.isRecord {
			// The slice holds *T wrappers whose raw handles were extracted into the C array — keep the
			// slice (and thus every wrapper) alive so no element's finalizer can fire mid-call.
			p.keepAlive = []string{nm}
		}
		p.cppPre = []string{
			fmt.Sprintf("%s<%s> %s_list; %s_list.reserve(%s_len);", cont, el.cppElem, nm, nm, nm),
			fmt.Sprintf("for (long long __i = 0; __i < %s_len; __i++) %s_list.%s(%s);", nm, nm, add, fmt.Sprintf(el.cppFrom, nm+"[__i]")),
		}
		return p, true
	}

	// associative container (QMap/QHash)<K,V> param -> map[K]V (keys array + vals array + len)
	if cont, isAssoc := assocContainer(ct.Head); ct.Kind == "template" && isAssoc && len(ct.Args) == 2 {
		t := ct // marshal the unwrapped container
		kEl := classifyListElem(t.Args[0], rv)
		vEl := classifyListElem(t.Args[1], rv)
		if !kEl.ok || !vEl.ok || kEl.isRecord {
			return paramPlan{}, false // record keys can't be Go map keys
		}
		kArrC := kEl.cArrC + "*"
		vArrC := vEl.cArrC + "*"
		if vEl.isRecord {
			vArrC = "void**"
		}
		p := paramPlan{
			cParams: []string{kArrC + " " + nm + "_keys", "void* " + nm + "_vals", "long long " + nm + "_len"},
			goParam: nm + " map[" + kEl.goElem + "]" + vEl.goElem,
			cppArg:  nm + "_map",
		}
		p.goPre = []string{
			fmt.Sprintf("%s_keys := make([]%s, 0, len(%s))", nm, kEl.cArrGo, nm),
			fmt.Sprintf("%s_vals := make([]%s, 0, len(%s))", nm, vEl.cArrGo, nm),
			fmt.Sprintf("for __k, __v := range %s {", nm),
			fmt.Sprintf("\t%s_keys = append(%s_keys, %s)", nm, nm, fmt.Sprintf(kEl.goToC, "__k")),
			fmt.Sprintf("\t%s_vals = append(%s_vals, %s)", nm, nm, fmt.Sprintf(vEl.goToC, "__v")),
			"}",
			fmt.Sprintf("var %s_kp *%s", nm, kEl.cArrGo),
			fmt.Sprintf("var %s_vp *%s", nm, vEl.cArrGo),
			fmt.Sprintf("if len(%s) > 0 {", nm),
			fmt.Sprintf("\t%s_kp = &%s_keys[0]", nm, nm),
			fmt.Sprintf("\t%s_vp = &%s_vals[0]", nm, nm),
			"}",
		}
		p.goArgs = []string{
			fmt.Sprintf("(%s)(unsafe.Pointer(%s_kp))", cArrPtrGo(kArrC), nm),
			fmt.Sprintf("unsafe.Pointer(%s_vp)", nm),
			fmt.Sprintf("C.longlong(len(%s))", nm),
		}
		p.cppPre = []string{
			fmt.Sprintf("%s<%s, %s> %s_map;", cont, kEl.cppElem, vEl.cppElem, nm),
			fmt.Sprintf("%s %s_vv = (%s)%s_vals;", vArrC, nm, vArrC, nm),
			fmt.Sprintf("for (long long __i = 0; __i < %s_len; __i++) %s_map.insert(%s, %s);",
				nm, nm, fmt.Sprintf(kEl.cppFrom, nm+"_keys[__i]"), fmt.Sprintf(vEl.cppFrom, nm+"_vv[__i]")),
		}
		if kEl.strElem { // free the malloc'd C strings the keys/values were marshalled into
			p.goPost = append(p.goPost, fmt.Sprintf("for _, __s := range %s_keys {", nm), "\t"+fmt.Sprintf(kEl.goFree, "__s"), "}")
		}
		if vEl.strElem {
			p.goPost = append(p.goPost, fmt.Sprintf("for _, __s := range %s_vals {", nm), "\t"+fmt.Sprintf(vEl.goFree, "__s"), "}")
		}
		if vEl.isRecord {
			p.keepAlive = []string{nm} // map values are *T wrappers — pin them across the call
		}
		return p, true
	}

	// Non-const reference to a scalar (int&/double&/qint64& ...): an IN/OUT parameter the method may read
	// AND modify (e.g. QAbstractSpinBox::validate(QString&, int& pos), QAtomicInt::testAndSet(..., int&)).
	// Surfaced as a value INPUT plus an extra Go return carrying the updated value — same tuple style as
	// pure out-params. (Record&/QString& are borrowed handles, handled by classifyScalar below.)
	if t.Kind == "ref" && !t.Const && t.Elem != nil && t.Elem.Kind != "record" {
		if esp := classifyScalar(*t.Elem, rv); esp.ok && esp.goType != "" && esp.cType != "void*" {
			if elemCpp := cppRetSpelling(*t.Elem); elemCpp != "" {
				return paramPlan{
					cParams:    []string{esp.cType + "* " + nm},
					goParam:    nm + " " + esp.goType,
					goPre:      []string{fmt.Sprintf("%s_io := %s", nm, fmt.Sprintf(esp.cgoArg, nm))},
					goArgs:     []string{"&" + nm + "_io"},
					outRetType: esp.goType,
					outRetExpr: fmt.Sprintf(esp.goRetWrap, nm+"_io"),
					cppArg:     nm + "_ref",
					cppPre:     []string{fmt.Sprintf("%s %s_ref = %s;", elemCpp, nm, fmt.Sprintf(esp.cppParamCast, "*"+nm))},
					cppPost:    []string{fmt.Sprintf("*%s = %s;", nm, fmt.Sprintf(esp.cppRetWrap, nm+"_ref"))},
				}, true
			}
		}
	}

	// Raw byte buffer: a non-const char*/uchar* param -> Go []byte (caller-provided). The paired length
	// argument binds separately as a normal integer (the singular-cppArg model can't merge them), so e.g.
	// QIODevice::read(char* data, qint64 maxSize) -> Read(data []byte, maxSize int64). (const char* is a
	// string, handled earlier; char* RETURNS stay dropped — no length to size a slice.)
	if t.Kind == "pointer" && !t.Const && t.Elem != nil && t.Elem.Kind == "builtin" &&
		(t.Elem.Name == "char" || t.Elem.Name == "unsigned char" || t.Elem.Name == "signed char") {
		return paramPlan{
			cParams: []string{"char* " + nm},
			goParam: nm + " []byte",
			goPre: []string{
				fmt.Sprintf("var %s_ptr *C.char", nm),
				fmt.Sprintf("if len(%s) > 0 {", nm),
				fmt.Sprintf("\t%s_ptr = (*C.char)(unsafe.Pointer(&%s[0]))", nm, nm),
				"}",
			},
			goArgs: []string{nm + "_ptr"},
			cppArg: "(" + t.Elem.Name + "*)" + nm, // char*/unsigned char*/signed char* — match the real pointee
		}, true
	}

	// const uchar* / const unsigned char* param -> Go []byte (a read-only input buffer the callee does
	// not modify; passed as the slice's base pointer, with NO separate length arg). Qt resource
	// registration QResource::registerResource(const uchar* rccData) takes this — `uchar` is a typedef
	// for `unsigned char`, so resolve through it. cppgo gap 5.
	if pe := t.Elem; t.Kind == "pointer" && t.Const && pe != nil {
		el := *pe
		for el.Kind == "typedef" && el.Canonical != nil {
			el = *el.Canonical
		}
		if el.Kind == "builtin" && el.Name == "unsigned char" {
			return paramPlan{
				cParams: []string{"const unsigned char* " + nm},
				goParam: nm + " []byte",
				goPre: []string{
					fmt.Sprintf("var %s_ptr *C.uchar", nm),
					fmt.Sprintf("if len(%s) > 0 {", nm),
					fmt.Sprintf("\t%s_ptr = (*C.uchar)(unsafe.Pointer(&%s[0]))", nm, nm),
					"}",
				},
				goArgs: []string{nm + "_ptr"},
				cppArg: nm,
			}, true
		}
	}

	// Output parameter: a non-const pointer to a scalar (bool*/int*/double*/qsizetype*...) that the method
	// WRITES — surfaced as an extra Go return value, e.g. toInt(bool* ok) -> (int, bool). (Qt convention:
	// a non-const scalar pointer is an out-param; a record pointer is a borrowed in-param, handled above.
	// char* is excluded — it's a string/byte buffer, not a single-scalar out-param.)
	if t.Kind == "pointer" && !t.Const && t.Elem != nil && t.Elem.Kind != "record" &&
		!(t.Elem.Kind == "builtin" && strings.Contains(t.Elem.Name, "char")) {
		if esp := classifyScalar(*t.Elem, rv); esp.ok && esp.goType != "" && esp.cType != "void*" {
			elemCpp := cppRetSpelling(*t.Elem)
			if elemCpp != "" {
				return paramPlan{
					cParams:    []string{esp.cType + "* " + nm},
					goPre:      []string{fmt.Sprintf("var %s_out %s", nm, cgoName(esp.cType))},
					goArgs:     []string{"&" + nm + "_out"},
					outRetType: esp.goType,
					outRetExpr: fmt.Sprintf(esp.goRetWrap, nm+"_out"),
					cppArg:     "&" + nm + "_local",
					cppPre:     []string{elemCpp + " " + nm + "_local{};"},
					cppPost:    []string{fmt.Sprintf("*%s = %s;", nm, fmt.Sprintf(esp.cppRetWrap, nm+"_local"))},
				}, true
			}
		}
	}

	sp := classifyScalar(t, rv)
	if !sp.ok || sp.goType == "" {
		return paramPlan{}, false
	}
	p := paramPlan{
		cParams: []string{sp.cType + " " + nm},
		goParam: nm + " " + sp.goType,
		goArgs:  []string{fmt.Sprintf(sp.cgoArg, nm)},
		cppArg:  fmt.Sprintf(sp.cppParamCast, nm),
	}
	if sp.handle {
		p.keepAlive = []string{nm} // wrapper arg: pin it so its finalizer can't free the C++ object mid-call
	}
	return p, true
}

// cArrPtrGo maps a C array type ("void**" / "int*") to the cgo pointer type for a Go cast.
func cArrPtrGo(arrC string) string {
	if arrC == "void**" {
		return "*unsafe.Pointer"
	}
	if arrC == "char**" { // QString keys: []*C.char -> **C.char (cgoName doesn't handle pointer elements)
		return "**C.char"
	}
	// e.g. "long long*" -> "*C.longlong"
	return "*" + cgoName(strings.TrimSuffix(arrC, "*"))
}

type retPlan struct {
	isVoid     bool
	cRetType   string
	cExtra     []string // extra C ABI params (out_len)
	goRetType  string
	cgoExtra   []string // extra cgo args for cExtra
	goPre      []string // before the call
	goPost     []string // after capturing __r
	goReturn   string   // final return expression ("" if void)
	cppRetType string
	cppPre     []string // after capturing __r (auto&& __r = call;)
	cppReturn  string   // return expression ("" if void)
}

func planReturn(t ir.Type, rv Resolver) (retPlan, bool) {
	// void
	if t.Kind == "builtin" && t.Name == "void" {
		return retPlan{isVoid: true, cRetType: "void", cppRetType: "void"}, true
	}
	// QString return -> Go string. The shim mallocs a UTF-8 copy (toUtf8) + writes its length
	// via an out-param; Go copies it out with GoStringN and frees the buffer.
	if isQString(t) {
		rp := retPlan{
			cRetType:   "char*",
			cExtra:     []string{"long long* __out_len"},
			goRetType:  "string",
			cgoExtra:   []string{"&__n"},
			goPre:      []string{"var __n C.longlong"},
			cppRetType: "char*",
		}
		deref := "__r."
		if t.Kind == "pointer" {
			deref = "__r->"
		}
		rp.cppPre = []string{
			"QByteArray __ba = " + deref + "toUtf8();",
			"*__out_len = __ba.size();",
			"char* __s = (char*)malloc(__ba.size() > 0 ? __ba.size() : 1);",
			"memcpy(__s, __ba.constData(), __ba.size());",
		}
		rp.cppReturn = "__s"
		rp.goPost = []string{"__out := C.GoStringN((*C.char)(unsafe.Pointer(__r)), C.int(__n))", "C.free(unsafe.Pointer(__r))"}
		rp.goReturn = "__out"
		return rp, true
	}

	// const char* return -> Go string. Qt owns the buffer (className()/typeName() are static), so copy
	// out with GoString and do NOT free.
	if isConstCharPtr(t) {
		return retPlan{
			cRetType:   "const char*",
			goRetType:  "string",
			cppRetType: "const char*",
			cppReturn:  "__r",
			goReturn:   "C.GoString(__r)",
		}, true
	}

	// Unwrap a const-ref and typedefs (const QObjectList&, QStringList = QList<QString>, ...) to the
	// underlying container template for the container-return marshalling below. A const-ref to a
	// container is copied out element-by-element, same as a by-value return.
	rt := t
	for rt.Kind == "ref" && rt.Const && rt.Elem != nil {
		rt = *rt.Elem
	}
	for rt.Kind == "typedef" && rt.Canonical != nil {
		rt = *rt.Canonical
	}

	// sequence container of QString (QStringList / QList<QString>) return -> Go []string. Each element is
	// a malloc'd NUL-terminated C string; Go copies it out (GoString) then frees it and the array.
	if _, _, isSeq := seqContainer(rt.Head); rt.Kind == "template" && isSeq && len(rt.Args) == 1 && isQString(rt.Args[0]) {
		rp := retPlan{
			cRetType:   "char**",
			cExtra:     []string{"long long* __out_len"},
			goRetType:  "[]string",
			cgoExtra:   []string{"&__n"},
			goPre:      []string{"var __n C.longlong"},
			cppRetType: "char**",
		}
		rp.cppPre = []string{
			"*__out_len = __r.size();",
			"char** __arr = (char**)malloc(sizeof(char*) * (__r.size() > 0 ? __r.size() : 1));",
			"long long __i = 0;",
			"for (auto&& __e : __r) { QByteArray __eb = __e.toUtf8(); char* __s = (char*)malloc(__eb.size() + 1); memcpy(__s, __eb.constData(), __eb.size()); __s[__eb.size()] = 0; __arr[__i++] = __s; }",
		}
		rp.cppReturn = "__arr"
		rp.goPost = []string{
			"__out := make([]string, int(__n))",
			"__sl := unsafe.Slice((**C.char)(unsafe.Pointer(__r)), int(__n))",
			"for __i := 0; __i < int(__n); __i++ {",
			"\t__out[__i] = C.GoString(__sl[__i])",
			"\tC.free(unsafe.Pointer(__sl[__i]))",
			"}",
			"C.free(unsafe.Pointer(__r))",
		}
		rp.goReturn = "__out"
		return rp, true
	}

	// sequence container (QList/QVector/QSet)<elem> return -> (void**|T*) + out_len
	if _, _, isSeq := seqContainer(rt.Head); rt.Kind == "template" && isSeq && len(rt.Args) == 1 {
		t := rt // marshal the unwrapped container
		el := classifyListElem(t.Args[0], rv)
		if !el.ok {
			return retPlan{}, false
		}
		arrC := el.cArrC + "*"
		if el.isRecord {
			arrC = "void**"
		}
		rp := retPlan{
			cRetType:   arrC,
			cExtra:     []string{"long long* __out_len"},
			goRetType:  "[]" + el.goElem,
			cgoExtra:   []string{"&__n"},
			goPre:      []string{"var __n C.longlong"},
			cppRetType: arrC,
		}
		rp.cppPre = []string{
			"*__out_len = __r.size();",
			fmt.Sprintf("%s __arr = (%s)malloc(sizeof(%s) * (__r.size() > 0 ? __r.size() : 1));", arrC, arrC, strings.TrimSuffix(arrC, "*")),
			"long long __i = 0;",
			fmt.Sprintf("for (auto&& __e : __r) __arr[__i++] = %s;", fmt.Sprintf(el.cppNew, "__e")), // range-for: works for QList/QVector/QSet
		}
		rp.cppReturn = "__arr"
		rp.goPost = []string{
			fmt.Sprintf("__out := make([]%s, int(__n))", el.goElem),
			fmt.Sprintf("__sl := unsafe.Slice((*%s)(unsafe.Pointer(__r)), int(__n))", el.cArrGo),
			"for __i := 0; __i < int(__n); __i++ {",
			fmt.Sprintf("\t__out[__i] = %s", fmt.Sprintf(el.goWrap, "__sl[__i]")),
			"}",
			"C.free(unsafe.Pointer(__r))",
		}
		rp.goReturn = "__out"
		return rp, true
	}

	// associative container (QMap/QHash)<K,V> return -> map[K]V. Keys come back as the C return
	// value, values via a void* out-param, count via out-param.
	if _, isAssoc := assocContainer(rt.Head); rt.Kind == "template" && isAssoc && len(rt.Args) == 2 {
		t := rt // marshal the unwrapped container
		kEl := classifyListElem(t.Args[0], rv)
		vEl := classifyListElem(t.Args[1], rv)
		// strElem (QString) is param-direction only — the return path's single-expression cppNew can't
		// malloc+copy a C string; a QMap<QString,V> RETURN therefore still drops (rare).
		if !kEl.ok || !vEl.ok || kEl.isRecord || kEl.strElem || vEl.strElem {
			return retPlan{}, false
		}
		kArrC := kEl.cArrC + "*"
		vArrC := vEl.cArrC + "*"
		if vEl.isRecord {
			vArrC = "void**"
		}
		rp := retPlan{
			cRetType:   kArrC,
			cExtra:     []string{"void** __out_vals", "long long* __out_len"},
			goRetType:  "map[" + kEl.goElem + "]" + vEl.goElem,
			cgoExtra:   []string{"&__vp", "&__n"},
			goPre:      []string{"var __n C.longlong", "var __vp unsafe.Pointer"},
			cppRetType: kArrC,
		}
		rp.cppPre = []string{
			"*__out_len = __r.size();",
			fmt.Sprintf("%s __keys = (%s)malloc(sizeof(%s) * (__r.size() > 0 ? __r.size() : 1));", kArrC, kArrC, strings.TrimSuffix(kArrC, "*")),
			fmt.Sprintf("%s __vals = (%s)malloc(sizeof(%s) * (__r.size() > 0 ? __r.size() : 1));", vArrC, vArrC, strings.TrimSuffix(vArrC, "*")),
			"long long __i = 0;",
			fmt.Sprintf("for (auto __it = __r.cbegin(); __it != __r.cend(); ++__it) { __keys[__i] = %s; __vals[__i] = %s; __i++; }",
				fmt.Sprintf(kEl.cppNew, "__it.key()"), fmt.Sprintf(vEl.cppNew, "__it.value()")),
			"*__out_vals = (void*)__vals;",
		}
		rp.cppReturn = "__keys"
		rp.goPost = []string{
			fmt.Sprintf("__ks := unsafe.Slice((*%s)(unsafe.Pointer(__r)), int(__n))", kEl.cArrGo),
			fmt.Sprintf("__vs := unsafe.Slice((*%s)(__vp), int(__n))", vEl.cArrGo),
			fmt.Sprintf("__out := make(map[%s]%s, int(__n))", kEl.goElem, vEl.goElem),
			"for __i := 0; __i < int(__n); __i++ {",
			fmt.Sprintf("\t__out[%s] = %s", fmt.Sprintf(kEl.goWrap, "__ks[__i]"), fmt.Sprintf(vEl.goWrap, "__vs[__i]")),
			"}",
			"C.free(unsafe.Pointer(__r))",
			"C.free(__vp)",
		}
		rp.goReturn = "__out"
		return rp, true
	}

	// std::optional<T> return -> Go (T, bool): the value via the C return, has_value() via a bool out-param.
	// (An empty optional yields the type's zero value + false.)
	if t.Kind == "template" && t.Head == "optional" && len(t.Args) == 1 {
		sp := classifyScalar(t.Args[0], rv)
		if !sp.ok || sp.goType == "" {
			return retPlan{}, false
		}
		zero := "(" + sp.cType + ")0" // valid for every C ABI scalar incl. multi-word ("unsigned long long") and void*
		return retPlan{
			cRetType:   sp.cType,
			cExtra:     []string{"int* __ok"},
			goRetType:  "(" + sp.goType + ", bool)",
			cgoExtra:   []string{"&__ok"},
			goPre:      []string{"var __ok C.int"},
			cppRetType: sp.cType,
			cppPre:     []string{"*__ok = __r.has_value() ? 1 : 0;"},
			cppReturn:  fmt.Sprintf("__r.has_value() ? %s : %s", fmt.Sprintf(sp.cppRetWrap, "*__r"), zero),
			goReturn:   fmt.Sprintf(sp.goRetWrap, "__r") + ", __ok != 0",
		}, true
	}

	// QPair/std::pair<A,B> return -> Go (A, B) multi-return. First comes back as the C return
	// value, second via a typed out-param.
	if t.Kind == "template" && isPair(t.Head) && len(t.Args) == 2 {
		a := classifyScalar(t.Args[0], rv)
		bp := classifyScalar(t.Args[1], rv)
		if !a.ok || a.goType == "" || !bp.ok || bp.goType == "" {
			return retPlan{}, false
		}
		bCgo := "unsafe.Pointer"
		if bp.cType != "void*" {
			bCgo = cgoName(bp.cType)
		}
		return retPlan{
			cRetType:   a.cType,
			cExtra:     []string{bp.cType + "* __out_b"},
			goRetType:  "(" + a.goType + ", " + bp.goType + ")",
			cgoExtra:   []string{"&__b"},
			goPre:      []string{"var __b " + bCgo},
			cppRetType: a.cType,
			cppPre:     []string{fmt.Sprintf("*__out_b = %s;", fmt.Sprintf(bp.cppRetWrap, "__r.second"))},
			cppReturn:  fmt.Sprintf(a.cppRetWrap, "__r.first"),
			goReturn:   fmt.Sprintf(a.goRetWrap, "__r") + ", " + fmt.Sprintf(bp.goRetWrap, "__b"),
		}, true
	}

	sp := classifyScalar(t, rv)
	if !sp.ok {
		return retPlan{}, false
	}
	return retPlan{
		cRetType:   sp.cType,
		goRetType:  sp.goType,
		cppRetType: sp.cType,
		cppReturn:  fmt.Sprintf(sp.cppRetWrap, "__r"),
		goReturn:   fmt.Sprintf(sp.goRetWrap, "__r"),
	}, true
}

// ---- assembled method ----

type method struct {
	cFunc, goName  string
	isCtor, isStat bool
	protCall       bool   // protected: call through the __Go subclass (which exposes it via using-decl)
	subclassRecv   bool   // call through the __Go subclass WITHOUT a using-decl (a protected virtual's <X>Default: a qualified, non-virtual base call only the subclass can reach)
	subclassFwd    string // C++ public forwarder member to emit in the __Go subclass body (gives the shim protected access to the base virtual)
	freeOpSym      string // free/friend operator bound as a method: call `(*(Cls*)self) <sym> (arg)` (binary) or `<sym>(*(Cls*)self)` (unary)
	convType       string // conversion operator target spelling: call `static_cast<convType>(*(Cls*)self)`
	fieldGet       string // public data-member getter: call expr is `recv-><field>` (no parens)
	fieldSet       string // public data-member setter: call expr is `recv-><field> = arg`
	baseCast       string // secondary-base accessor: call expr is `static_cast<baseCast*>((Cls*)self)` (gap 1)
	variantWrap    bool   // ToQVariant() helper: call expr is `QVariant::fromValue(*(Cls*)self)` (gap 2)
	cppCall        string
	params         []paramPlan
	ret            retPlan
}

func plan(m ir.Method, isCtor bool, rv Resolver) (method, bool) {
	mm := method{isStat: m.Static, isCtor: isCtor, cppCall: m.Name}
	if m.Conversion {
		// call becomes static_cast<convType>(*(Cls*)self) in emitCpp.
		rt := m.Ret
		// A class-NESTED conversion-target typedef (operator DescriptorType, = QSocketDescriptor::int)
		// spells only its bare alias "DescriptorType", which won't resolve in the free shim. When the
		// canonical is a builtin, static_cast to the canonical (int) is equivalent and self-qualifying.
		if rt.Kind == "typedef" && rt.Canonical != nil && rt.Canonical.Kind == "builtin" {
			rt = *rt.Canonical
		}
		mm.convType = cppRetSpelling(rt)
	}
	if !isCtor {
		rp, ok := planReturn(m.Ret, rv)
		if !ok {
			return mm, false
		}
		mm.ret = rp
	} else {
		mm.ret = retPlan{cRetType: "void*", cppRetType: "void*"} // ctor returns the handle
	}
	for i, p := range m.Params {
		nm := goIdent(p.Name)
		if nm == "" {
			nm = fmt.Sprintf("arg%d", i+1)
		}
		pp, ok := planParam(nm, p.Type, rv)
		if !ok {
			return mm, false
		}
		mm.params = append(mm.params, pp)
	}
	return mm, true
}

// ---- signals (receive side: register a Go callback that fires on emit) ----
