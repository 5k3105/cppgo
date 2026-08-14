package gen

import (
	"fmt"
	"strings"

	"github.com/5k3105/cppgo/ir"
)

// A signal carries its args C++ -> Go, which is the same direction as a return value, so each arg
// reuses classifyScalar's cppRetWrap (C++ value -> C ABI) and goRetWrap (C ABI -> Go).
type sigArg struct {
	name      string
	goType    string   // Go callback param type
	cppType   string   // C++ lambda param type (must accept the real signal arg)
	goConv    string   // Go expr: cgo trampoline param -> Go value
	cParams   []string // C ABI param decls the trampoline receives
	cgoParams []string // Go trampoline param decls (cgo types)
	cppPre    []string // C++ lambda-body statements before the trampoline call
	cppArgs   []string // C++ values passed to the trampoline
}

type signal struct {
	connectGo  string // Go method name, e.g. ConnectTimeout
	connectC   string // C connect-shim, e.g. QTimer_connect_timeout
	tramp      string // //export Go trampoline name (called from the C++ lambda)
	member     string // C++ signal member name (for &Class::member)
	overloaded bool   // name is overloaded -> &Class::member is ambiguous, disambiguate with a static_cast
	args       []sigArg
	exposed    int // # of leading args surfaced to Go (< len(args) drops trailing default-valued args; the
	// C++ lambda still declares ALL args to match &Class::member, but forwards only the first `exposed`)
}

func planSigArg(nm string, t ir.Type, rv Resolver) (sigArg, bool) {
	if isQString(t) {
		return sigArg{
			name: nm, goType: "string", cppType: "const QString&",
			cParams:   []string{"const char* " + nm, "long long " + nm + "_len"},
			cgoParams: []string{nm + " *C.char", nm + "_len C.longlong"},
			cppPre:    []string{fmt.Sprintf("QByteArray %s_ba = %s.toUtf8();", nm, nm)},
			cppArgs:   []string{"(char*)" + nm + "_ba.constData()", nm + "_ba.size()"},
			goConv:    fmt.Sprintf("C.GoStringN(%s, C.int(%s_len))", nm, nm),
		}, true
	}
	// Sequence-container arg (QModelIndexList = QList<QModelIndex>, ...) in a virtual/signal override: the
	// C++ lambda receives it by const-ref and forwards its elements as a BORROWED void* array + length; the
	// Go callback gets a []*Elem valid only for the call. Each element is a borrowed handle (NOT owned —
	// it points into the live C++ container; an owned wrap would double-free). (gap 4: container-arg overrides.)
	{
		ct := t
		for ct.Kind == "ref" && ct.Const && ct.Elem != nil {
			ct = *ct.Elem
		}
		for ct.Kind == "typedef" && ct.Canonical != nil {
			ct = *ct.Canonical
		}
		if cont, _, isSeq := seqContainer(ct.Head); ct.Kind == "template" && isSeq && len(ct.Args) == 1 && ct.Args[0].Kind == "record" {
			if gt, bare, cpp, pfx, ok := rv.rec(ct.Args[0].USR); ok {
				wrap := pfx + "UnsafeNew" + bare
				goConv := "func() []*" + gt + " { if " + nm + " == nil || " + nm + "_len == 0 { return nil }; __sl := unsafe.Slice((*unsafe.Pointer)(" + nm + "), int(" + nm + "_len)); __o := make([]*" + gt + ", int(" + nm + "_len)); for __i := range __o { __o[__i] = " + wrap + "(__sl[__i]) }; return __o }()"
				return sigArg{
					name: nm, goType: "[]*" + gt, cppType: "const " + cont + "<" + cpp + ">&",
					cParams:   []string{"void** " + nm, "long long " + nm + "_len"},
					cgoParams: []string{nm + " unsafe.Pointer", nm + "_len C.longlong"},
					cppPre: []string{
						"std::vector<void*> " + nm + "_v; " + nm + "_v.reserve(" + nm + ".size());",
						"for (long long __i = 0; __i < " + nm + ".size(); __i++) " + nm + "_v.push_back((void*)&" + nm + "[__i]);",
					},
					cppArgs: []string{nm + "_v.data()", "(long long)" + nm + ".size()"},
					goConv:  goConv,
				}, true
			}
		}
	}

	// Bound record arg -> a BORROWED Go handle (the callback does not own the signal/virtual's
	// argument; it is valid only for the duration of the call). Crosses as a void* handle.
	{
		usr, byPtr, byVal := "", false, false
		switch t.Kind {
		case "record":
			usr, byVal = t.USR, true
		case "ref":
			if t.Elem != nil && t.Elem.Kind == "record" {
				usr = t.Elem.USR
			}
		case "pointer":
			if t.Elem != nil && t.Elem.Kind == "record" {
				usr, byPtr = t.Elem.USR, true
			}
		}
		if gt, bare, cpp, pfx, ok := rv.rec(usr); ok && usr != "" {
			// Preserve the pointee's const-qualification: a virtual override's C++ signature must
			// match the base EXACTLY (e.g. dropMimeData(const QMimeData*, ...)), or it silently
			// fails to override. t.Const is the pointer/ref pointee-const flag from the frontend.
			cq := ""
			if t.Const {
				cq = "const "
			}
			cppType, cppArg := cq+cpp+"&", "(void*)&"+nm
			if byPtr {
				cppType, cppArg = cq+cpp+"*", "(void*)"+nm
			} else if byVal {
				cppType = cpp
			}
			return sigArg{
				name: nm, goType: "*" + gt, cppType: cppType,
				cParams:   []string{"void* " + nm},
				cgoParams: []string{nm + " unsafe.Pointer"},
				cppArgs:   []string{cppArg},
				goConv:    fmt.Sprintf("%sUnsafeNew%s(%s)", pfx, bare, nm),
			}, true
		}
	}
	// void* argument in an override (e.g. QAbstractNativeEventFilter::nativeEventFilter(..., void* message,
	// ...)) -> unsafe.Pointer passed straight through to the callback.
	if t.Kind == "pointer" && t.Elem != nil && t.Elem.Kind == "builtin" && t.Elem.Name == "void" {
		cq := ""
		if t.Const {
			cq = "const "
		}
		return sigArg{
			name: nm, goType: "unsafe.Pointer", cppType: cq + "void*",
			cParams:   []string{"void* " + nm},
			cgoParams: []string{nm + " unsafe.Pointer"},
			cppArgs:   []string{"(void*)" + nm},
			goConv:    nm,
		}, true
	}
	// Writable scalar reference/pointer in an override: a non-const T& (in/out, e.g. validate(QString&,
	// int& pos)) or T* (out, e.g. QAccessibleTextInterface::selection(int, int* start, int* end),
	// nativeEventFilter(..., qintptr* result)). Expose a writable *GoFixed the callback aliases in place
	// (valid for the call) — no separate writeback. (QString&/record& are borrowed handles, above.)
	if (t.Kind == "ref" || t.Kind == "pointer") && !t.Const && t.Elem != nil && t.Elem.Kind != "record" {
		// Resolve typedefs (qintptr -> long long) and reject byte-sized pointees: aliasing a bool (1 byte)
		// or char through a 4-byte *int32 reads/writes out of bounds. Enums are int-backed -> safe.
		et := *t.Elem
		for et.Kind == "typedef" && et.Canonical != nil {
			et = *et.Canonical
		}
		byteSized := et.Kind == "builtin" && (et.Name == "bool" || strings.Contains(et.Name, "char"))
		if esp := classifyScalar(*t.Elem, rv); esp.ok && esp.cType != "void*" && !byteSized {
			elemCpp := cppRetSpelling(*t.Elem)
			if gf := cTypeGoFixed(esp.cType); elemCpp != "" && gf != "" {
				cppT, addr := elemCpp+"&", "&"+nm // T& -> address of the reference
				if t.Kind == "pointer" {
					cppT, addr = elemCpp+"*", nm // T* -> the pointer itself
				}
				return sigArg{
					name: nm, goType: "*" + gf, cppType: cppT,
					cParams:   []string{esp.cType + "* " + nm},
					cgoParams: []string{nm + " *" + cgoName(esp.cType)},
					// reinterpret the (enum*/typedef*) address as the C ABI scalar pointer (same width)
					cppArgs: []string{"(" + esp.cType + "*)" + addr},
					goConv:  fmt.Sprintf("(*%s)(unsafe.Pointer(%s))", gf, nm),
				}, true
			}
		}
	}

	sp := classifyScalar(t, rv)
	if !sp.ok || sp.goType == "" || sp.cType == "void*" {
		return sigArg{}, false // unbound args are deferred to a later slice
	}
	// cppType is the lambda/override param type and must spell the declared type EXACTLY. t.Name is empty
	// for a typedef or QFlags<> template, so use a proper spelling — otherwise a typedef/QFlags ARGUMENT
	// (Qt::KeyboardModifiers, SelectionFlags) wrongly drops the signal/virtual.
	cppT := cppRetSpelling(t)
	if cppT == "" {
		return sigArg{}, false
	}
	return sigArg{
		name: nm, goType: sp.goType, cppType: cppT,
		cParams:   []string{sp.cType + " " + nm},
		cgoParams: []string{nm + " " + cgoName(sp.cType)},
		cppArgs:   []string{fmt.Sprintf(sp.cppRetWrap, nm)},
		goConv:    fmt.Sprintf(sp.goRetWrap, nm),
	}, true
}

func planSignal(m ir.Method, rv Resolver) (signal, bool) {
	params := m.Params
	// Qt signals carry a trailing private QPrivateSignal tag arg; callers never pass it and a
	// functor may take fewer args, so drop it.
	if n := len(params); n > 0 && strings.Contains(params[n-1].Type.Name, "QPrivateSignal") {
		params = params[:n-1]
	}
	s := signal{member: m.Name}
	for i, p := range params {
		nm := goIdent(p.Name)
		if nm == "" {
			nm = fmt.Sprintf("arg%d", i+1)
		}
		a, ok := planSigArg(nm, p.Type, rv)
		if !ok {
			return signal{}, false
		}
		s.args = append(s.args, a)
	}
	s.exposed = len(s.args)
	return s, true
}

// ---- virtual overrides (let Go provide a C++ virtual's implementation) ----

// A vmethod is a virtual the Go side may override. This slice handles no-arg virtuals returning void
// or a scalar/enum (records/QString returns and args are a later slice). The value flows Go -> C ABI
// -> C++ (the opposite of a signal arg), so it reuses scalarPlan's cgoArg (Go->C ABI) and
// cppParamCast (C ABI -> C++).
type vmethod struct {
	member       string // C++ virtual name (used for the C++ `override` declaration + base call)
	slotKey      string // per-instance registry key — unique per OVERLOAD (member is shared by overloads)
	slotID       int64  // FNV-1a hash of slotKey — the registry key that crosses the ABI (no strings on the hot path)
	idx          int    // position in this class's virts list == index into the __Go subclass's __goOn override mask
	onGo         string // Go setter name, e.g. OnArea
	tramp        string // //export trampoline name
	isConst      bool
	isVoid       bool
	isPure       bool     // a pure virtual: the "not handled" path must NOT call the (abstract) base
	retQString   bool     // return crosses as UTF-8 char*+len (needs its own out-param + free)
	retList      bool     // return is a sequence container (QList/QVector/QSet)<elem> -> Go []elem via a malloc'd C array + out-len
	lstr         bool     // retList whose element is QString -> Go []string (char* array; idiomatic, matches the regular-method return)
	lel          listElem // element marshalling for retList (non-string elements)
	lcont        string   // C++ container head for retList ("QList"/"QVector"/"QSet")
	ladd         string   // element-add method for retList ("append"/"insert")
	cppRet       string   // C++ return type spelling
	cType        string   // C ABI return type
	cgoRet       string   // Go trampoline return type
	retZero      string   // zero literal for the not-handled path
	goRet        string   // Go callback return type
	goToC        string   // fmt: Go value -> cgo return
	cppCast      string   // fmt: C ABI value -> C++ return
	args         []sigArg
	retRecordUSR string // USR of a by-value record return (for the covariant-return check); "" otherwise
}

// sigHasInaccessibleEnum reports whether a PARAM names an enum a FREE function can't spell — a
// protected/private NESTED enum (e.g. QAbstractItemView::CursorAction, QGraphicsItem::Extension). A
// <X>Default / protCall free shim names its param types, so such an enum can't appear there. (Only params:
// an enum RETURN is always fine — the shim deduces it via `auto` and casts TO int, never spelling the
// type, so e.g. dropIndicatorPosition()->State binds.) Public/top-level enums are spellable -> not flagged.
// Unknown enums (not in EnumPublic, e.g. unbound) are treated as accessible to avoid over-skipping.
func sigHasInaccessibleEnum(m ir.Method, rv Resolver) bool {
	for _, p := range m.Params {
		if p.Type.Kind == "enum" {
			if pub, known := rv.EnumPublic[p.Type.USR]; known && !pub {
				return true
			}
		}
	}
	return false
}

// covariantRecordReturn reports whether a virtual's by-value record return (retUSR) would trip GCC's
// "invalid covariant return type" when overridden in classUSR's __Go subclass — the returned record
// derives from classUSR's OWN base hierarchy (KConfigBase::groupImpl -> KConfigGroup : KConfigBase). A
// record unrelated to the class (QAbstractItemView::moveCursor -> QModelIndex) is safe to override.
func covariantRecordReturn(retUSR, classUSR string, rv Resolver) bool {
	if retUSR == "" {
		return false
	}
	hier := map[string]bool{classUSR: true}
	for a := range rv.Ancestors[classUSR] {
		hier[a] = true
	}
	if hier[retUSR] {
		return true
	}
	for a := range rv.Ancestors[retUSR] {
		if hier[a] {
			return true
		}
	}
	return false
}

func planVirt(m ir.Method, rv Resolver) (vmethod, bool) {
	v := vmethod{member: m.Name, isConst: m.Const}
	for i, p := range m.Params {
		nm := goIdent(p.Name)
		if nm == "" {
			nm = fmt.Sprintf("arg%d", i+1)
		}
		a, ok := planSigArg(nm, p.Type, rv)
		if !ok {
			return vmethod{}, false
		}
		v.args = append(v.args, a)
	}
	// A Go override CAN return a POINTER to a record: it allocates the object and returns its handle,
	// which Qt takes ownership of — e.g. QStyledItemDelegate::createEditor() -> QWidget*,
	// QAbstractItemModel::mimeData() -> QMimeData*. (Differs from a REFERENCE return, which has no stable
	// owner.) The C++ override returns (T*)handle; the not-handled path falls through to the base.
	if m.Ret.Kind == "pointer" && m.Ret.Elem != nil && m.Ret.Elem.Kind == "record" {
		if gt, _, cpp, _, ok := rv.rec(m.Ret.Elem.USR); ok {
			cq := "" // preserve pointee const so the override return type matches the base EXACTLY
			if m.Ret.Const {
				cq = "const "
			}
			v.cppRet, v.cType, v.cgoRet, v.retZero = cq+cpp+"*", "void*", "unsafe.Pointer", "nil"
			v.goRet, v.goToC, v.cppCast = "*"+gt, "%s.UnsafePointer()", "("+cq+cpp+"*)%s"
			return v, true
		}
		return vmethod{}, false
	}
	// An override must return EXACTLY what the base declares; a Go override can't return a reference
	// (no stable storage / owner) — e.g. KCompletion::lastMatch() -> const QString&. Skip those (the
	// non-virtual method still binds and returns a copy).
	if m.Ret.Kind == "ref" || m.Ret.Kind == "pointer" || m.Ret.Kind == "rref" {
		return vmethod{}, false
	}
	// Sequence-container return (QList/QVector/QSet<elem>, incl. typedefs like QStringList): the Go override
	// returns []elem; the trampoline marshals it to a malloc'd C array + out-len; the C++ override rebuilds
	// the container and frees the array. (Mirrors the regular-method container return, but Go->C++.) Enum
	// elements aren't bindable until gap C extends classifyListElem, so QList<enum> still drops here.
	rt := m.Ret
	for rt.Kind == "typedef" && rt.Canonical != nil {
		rt = *rt.Canonical
	}
	if cont, add, isSeq := seqContainer(rt.Head); rt.Kind == "template" && isSeq && len(rt.Args) == 1 {
		// QString element -> idiomatic []string (char* array), matching the regular-method return; other
		// elements go through classifyListElem ([]*Record / []scalar; enum waits on gap C).
		if isQString(rt.Args[0]) {
			v.retList, v.lstr, v.lcont, v.ladd = true, true, cont, add
			v.cppRet = cont + "<QString>"
			v.goRet = "[]string"
			return v, true
		}
		el := classifyListElem(rt.Args[0], rv)
		if !el.ok {
			return vmethod{}, false
		}
		v.retList, v.lel, v.lcont, v.ladd = true, el, cont, add
		v.cppRet = cont + "<" + el.cppElem + ">"
		v.goRet = "[]" + el.goElem
		return v, true
	}
	switch {
	case m.Ret.Kind == "builtin" && m.Ret.Name == "void":
		v.isVoid = true
		v.cppRet = "void"
		return v, true
	case isQString(m.Ret):
		v.retQString = true
		v.cppRet = "QString"
		v.goRet = "string"
		return v, true
	}
	// Bound record return by value: Go returns *T, the override copies it (`*(T*)handle`). Only the
	// plain value/record form; ref/pointer returns from an override have no clear owner, so skip.
	if m.Ret.Kind == "record" {
		// Record returned BY VALUE. Record the USR so the caller can apply the covariant-return check (a
		// return type deriving from the class's own base hierarchy trips GCC "invalid covariant return
		// type"); a record unrelated to the class — moveCursor() -> QModelIndex — overrides fine.
		v.retRecordUSR = m.Ret.USR
		if gt, _, cpp, _, ok := rv.rec(m.Ret.USR); ok {
			v.cppRet, v.cType, v.cgoRet, v.retZero = cpp, "void*", "unsafe.Pointer", "nil"
			v.goRet, v.goToC, v.cppCast = "*"+gt, "%s.UnsafePointer()", "*("+cpp+"*)%s"
			return v, true
		}
		return vmethod{}, false
	}
	sp := classifyScalar(m.Ret, rv)
	if !sp.ok || sp.goType == "" || sp.cType == "void*" {
		return vmethod{}, false
	}
	// The override's C++ return type must spell the base's EXACTLY. m.Ret.Name is empty for a typedef or
	// a QFlags<> template (the frontend stores those as alias/head, not name), so use a proper spelling —
	// otherwise typedef/QFlags returns (Qt::Orientations, qsizetype, qreal) wrongly drop the override.
	v.cppRet = cppRetSpelling(m.Ret)
	if v.cppRet == "" {
		return vmethod{}, false
	}
	v.cType = sp.cType
	v.cgoRet = cgoName(sp.cType)
	v.retZero = "0"
	v.goRet = sp.goType
	v.goToC = sp.cgoArg
	v.cppCast = sp.cppParamCast
	return v, true
}

// cppRetSpelling gives a C++ type spelling for a scalar/enum/QFlags override return — resolving a
// typedef to its spelled alias and a QFlags<> template to QFlags<Arg> — since ir.Type.Name is only set
// for plain record/enum/builtin nodes.
func cppRetSpelling(t ir.Type) string {
	switch t.Kind {
	case "typedef":
		// A class-nested QFlags typedef (e.g. QInputDevice::Capabilities) spelled by its bare alias
		// ("Capabilities") won't compile in a free function / lambda param; prefer the canonical
		// QFlags<Enum> (the enum arg is fully qualified), which is self-qualifying everywhere.
		if t.Canonical != nil && t.Canonical.Kind == "template" && t.Canonical.Head == "QFlags" {
			return cppRetSpelling(*t.Canonical)
		}
		if t.Alias != "" {
			return t.Alias
		}
		if t.Canonical != nil {
			return cppRetSpelling(*t.Canonical)
		}
	case "template":
		if t.Head == "QFlags" && len(t.Args) == 1 && t.Args[0].Name != "" {
			return "QFlags<" + t.Args[0].Name + ">"
		}
		return t.Head
	}
	return t.Name
}

// convGoIdent turns a conversion target type into a Go identifier suffix for the To<Type>() method name:
// it keeps only [A-Za-z0-9] from the C++ spelling and upper-cases the first letter of each run, so
// "bool" -> "Bool", "QStringView" -> "QStringView", "Qt::CaseSensitivity" -> "QtCaseSensitivity".
func convGoIdent(t ir.Type) string {
	s := cppRetSpelling(t)
	var b strings.Builder
	up := true
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
			if up {
				r -= 'a' - 'A'
			}
			b.WriteRune(r)
			up = false
		case (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			up = false
		default:
			up = true // separator (::, <>, space, *) -> camel-case the next run
		}
	}
	return b.String()
}
