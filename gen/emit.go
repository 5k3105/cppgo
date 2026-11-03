package gen

import (
	"fmt"
	"github.com/5k3105/cppgo/ir"
	"sort"
	"strings"
)

func cSig(self bool, m method) string {
	var ps []string
	if self {
		ps = append(ps, "void* self")
	}
	for _, p := range m.params {
		ps = append(ps, p.cParams...)
	}
	ps = append(ps, m.ret.cExtra...)
	return strings.Join(ps, ", ")
}

func emitHeader(cn string, ms []method, sigs []signal, virts []vmethod, parentCtor bool, base baseEmit, isQObject, hasMeta bool, backend string, chainLen int) string {
	var b strings.Builder
	fmt.Fprint(&b, "#pragma once\n#include <stddef.h>\n#ifdef __cplusplus\nextern \"C\" {\n#endif\n\n")
	if base.has {
		// Casts a derived pointer to its primary-base subobject pointer (Go embeds *base and stores the
		// adjusted handle). A free function, not a cast in Go, so multiple-inheritance offsets are correct.
		fmt.Fprintf(&b, "void* %s(void* self);\n", base.cFunc)
	}
	if chainLen >= 2 {
		// Fills out[0..chainLen-1] with every primary-base subobject pointer in ONE call, so bind<Cls>
		// costs one crossing regardless of inheritance depth (instead of one <cn>_base call per level).
		fmt.Fprintf(&b, "void %s_chain(void* self, void** out);\n", cn)
	}
	if len(virts) > 0 {
		// Flips one bit of a __Go instance's override mask: an unregistered virtual then never crosses
		// into Go (the C++ override falls straight through to the base implementation).
		fmt.Fprintf(&b, "void %s__set_override(void* self, int idx);\n", cn)
	}
	if isQObject && hasMeta && cn != "QObject" {
		fmt.Fprintf(&b, "void* %s_fromQObject(void* o);\n", cn) // checked qobject_cast downcast
	}
	for _, m := range ms {
		if m.isCtor {
			sig := cSig(false, m)
			if len(virts) > 0 { // subclassable ctor takes a leading override id
				if sig == "" {
					sig = "long long __goid"
				} else {
					sig = "long long __goid, " + sig
				}
			}
			fmt.Fprintf(&b, "void* %s(%s);\n", m.cFunc, sig)
			continue
		}
		if backend == "purego" && pgPackable(m) { // packed-args shim signature (matches emitCpp)
			fmt.Fprintf(&b, "%s %s(void* self, void* __ap);\n", m.ret.cRetType, m.cFunc)
			continue
		}
		fmt.Fprintf(&b, "%s %s(%s);\n", m.ret.cRetType, m.cFunc, cSig(!m.isStat, m))
	}
	for _, s := range sigs {
		fmt.Fprintf(&b, "void %s(void* self, long long id);\n", s.connectC)
	}
	if len(sigs) > 0 {
		fmt.Fprintf(&b, "void %s__disconnect(long long id);\n", cn)
	}
	if len(virts) > 0 {
		if isAppClass(cn) {
			fmt.Fprintf(&b, "void* %s_new_args(long long __goid, int* argc, char** argv);\n", cn)
		} else if parentCtor {
			fmt.Fprintf(&b, "void* %s__Go_new(long long id, void* parent);\n", cn)
		} else {
			fmt.Fprintf(&b, "void* %s__Go_new(long long id);\n", cn)
		}
	}
	if len(virts) == 0 && isAppClass(cn) { // plain app ctor (see emitCpp)
		fmt.Fprintf(&b, "void* %s_new_args(int* argc, char** argv);\n", cn)
	}
	fmt.Fprintf(&b, "void %s_delete(void* self);\n\n#ifdef __cplusplus\n}\n#endif\n", cn)
	return b.String()
}

func emitCpp(cls, cn, header string, ms []method, sigs []signal, virts []vmethod, parentCtor bool, refHeaders []string, publicDtor bool, base baseEmit, isQObject, hasMeta bool, backend string, chain []baseEmit) string {
	purego := backend == "purego"
	var b strings.Builder
	inc := "<" + cls + ">"
	if header != "" {
		inc = includeSpelling(header)
	}
	// The common marshalling headers (containers, QString, QObject casts, the connection map) are
	// force-included once via the precompiled cppgo_pch.h (set in the build CXXFLAGS), so the shim emits
	// only its OWN class header here + the referenced-type headers below. NOT the module umbrella.
	fmt.Fprintf(&b, "#include %s\n", inc)
	// ...plus exactly the headers of the types this shim references (bases, params, returns), so cross-
	// type uses (e.g. QVariant's shim touching QModelIndex) see complete types — and nothing more.
	for _, h := range refHeaders {
		fmt.Fprintf(&b, "#include %s\n", includeSpelling(h))
	}
	fmt.Fprintf(&b, "#include \"gen_%s.h\"\n", strings.ToLower(cn))
	if len(sigs) > 0 {
		// The per-class connection registry + its lock. Included HERE (not via a shared PCH) so every
		// signal-bearing shim TU is self-contained regardless of which PCH the build wraps around it.
		fmt.Fprint(&b, "#include <unordered_map>\n#include <mutex>\n")
	}
	if purego && (len(sigs) > 0 || len(virts) > 0) {
		// purego connect-lambdas / virtual overrides call per-shape dispatch pointers declared here.
		fmt.Fprint(&b, "#include \"gen_purego_cb.h\"\n")
	}
	// NOTE: _cgo_export.h (the //export trampoline decls that signal lambdas + virtual overrides call) is
	// NOT included here — it has no include guard, so in a unity TU it must appear exactly once. The unit
	// writer (cmd/gen) prepends it once per aggregate .cpp when any class in the unit has exports.
	fmt.Fprint(&b, "\n")
	// A Go-subclass of the C++ type: each overridable virtual calls into Go if an override was
	// registered for this instance's id, else falls through to the base implementation.
	if len(virts) > 0 {
		// A single variadic-template ctor forwards ANY base-ctor argument list to the base (the leading
		// `long long id` disambiguates it from copy/move), so every New<Cls>(args) can build the
		// overridable subclass — universal overridability, like miqt's MiqtVirtual<Cls>.
		ctor := fmt.Sprintf("template<typename... A> %s__Go(long long id, A&&... a) : %s(std::forward<A>(a)...), __goId(id) {}", cn, cls)
		fmt.Fprintf(&b, "class %s__Go : public %s {\npublic:\n\tlong long __goId;\n", cn, cls)
		// Per-instance override mask, one byte per virtual, flipped by <cn>__set_override when Go
		// registers an On<X>. An UNREGISTERED virtual (the overwhelmingly common case for paint/event/
		// resize on plain widgets) then costs a single local byte test and falls straight to the base —
		// no C->Go crossing, no lock, no registry lookup.
		fmt.Fprintf(&b, "\tunsigned char __goOn[%d] = {};\n\t%s\n", len(virts), ctor)
		// Expose protected base methods (e.g. createIndex) as public in the subclass so their shims can
		// call them through a (cn__Go*) cast.
		protSeen := map[string]bool{}
		for _, m := range ms {
			if m.protCall && !protSeen[m.cppCall] {
				protSeen[m.cppCall] = true
				fmt.Fprintf(&b, "\tusing %s::%s;\n", cls, m.cppCall)
			}
			if m.subclassFwd != "" { // public forwarder so the <X>Default shim can reach a protected base virtual
				fmt.Fprint(&b, m.subclassFwd)
			}
		}
		for _, v := range virts {
			if purego { // dispatch-pointer override (skips unsupported shapes, in lockstep with the Go side)
				fmt.Fprint(&b, pgVirtOverrideCpp(cls, v))
				continue
			}
			cst := ""
			if v.isConst {
				cst = " const"
			}
			var lambdaParams, trampArgs, baseArgs, pre []string
			for _, a := range v.args {
				lambdaParams = append(lambdaParams, a.cppType+" "+a.name)
				pre = append(pre, a.cppPre...)
				trampArgs = append(trampArgs, a.cppArgs...)
				baseArgs = append(baseArgs, a.name)
			}
			callArgs := append([]string{"__goId"}, trampArgs...)
			// A pure virtual has no callable base, so its "not handled" path returns a default
			// (value-initialized); a regular virtual falls through to the base implementation.
			fallVoid := fmt.Sprintf("%s::%s(%s)", cls, v.member, strings.Join(baseArgs, ", "))
			fallVal := fallVoid
			if v.isPure {
				fallVoid = "return"
				switch {
				case strings.HasSuffix(v.cppRet, "*"): // pointer -> nullptr (not `T*{}`)
					fallVal = "nullptr"
				case strings.Contains(v.cppRet, " "): // multi-word builtin (unsigned long long): `T{}` is a syntax error
					fallVal = "(" + v.cppRet + ")0"
				default:
					fallVal = v.cppRet + "{}"
				}
			}
			// The whole C->Go crossing is gated on this instance's override mask: no registered Go
			// override for this virtual -> one byte test, then straight to the base implementation.
			fmt.Fprintf(&b, "\t%s %s(%s)%s override {\n\t\tif (__goOn[%d]) {\n\t\t\tint __h = 0;\n", v.cppRet, v.member, strings.Join(lambdaParams, ", "), cst, v.idx)
			for _, p := range pre {
				fmt.Fprintf(&b, "\t\t\t%s\n", p)
			}
			switch {
			case v.isVoid:
				ca := append(callArgs, "&__h")
				fmt.Fprintf(&b, "\t\t\t%s(%s);\n\t\t\tif (__h) return;\n\t\t}\n\t\t%s;\n\t}\n", v.tramp, strings.Join(ca, ", "), fallVoid)
			case v.retQString:
				ca := append(callArgs, "&__len", "&__h")
				fmt.Fprintf(&b, "\t\t\tlong long __len = 0;\n\t\t\tchar* __s = %s(%s);\n\t\t\tif (__h) { QString __r = QString::fromUtf8(__s, __len); free(__s); return __r; }\n\t\t}\n\t\treturn %s;\n\t}\n",
					v.tramp, strings.Join(ca, ", "), fallVal)
			case v.retList:
				ca := append(callArgs, "&__len", "&__h")
				if v.lstr { // char* array -> QString list; free each C.CString then the array
					fmt.Fprintf(&b, "\t\t\tlong long __len = 0;\n\t\t\tvoid* __a = %s(%s);\n\t\t\tif (__h) {\n\t\t\t\t%s __r; __r.reserve(__len);\n\t\t\t\tchar** __aa = (char**)__a;\n\t\t\t\tfor (long long __i = 0; __i < __len; __i++) { __r.%s(QString::fromUtf8(__aa[__i])); free(__aa[__i]); }\n\t\t\t\tif (__a) free(__a);\n\t\t\t\treturn __r;\n\t\t\t}\n\t\t}\n\t\treturn %s;\n\t}\n",
						v.tramp, strings.Join(ca, ", "), v.cppRet, v.ladd, fallVal)
				} else {
					arrCType := v.lel.cArrC
					if v.lel.isRecord {
						arrCType = "void*"
					}
					cppFromElem := fmt.Sprintf(v.lel.cppFrom, "__aa[__i]")
					fmt.Fprintf(&b, "\t\t\tlong long __len = 0;\n\t\t\tvoid* __a = %s(%s);\n\t\t\tif (__h) {\n\t\t\t\t%s __r; __r.reserve(__len);\n\t\t\t\t%s* __aa = (%s*)__a;\n\t\t\t\tfor (long long __i = 0; __i < __len; __i++) __r.%s(%s);\n\t\t\t\tif (__a) free(__a);\n\t\t\t\treturn __r;\n\t\t\t}\n\t\t}\n\t\treturn %s;\n\t}\n",
						v.tramp, strings.Join(ca, ", "), v.cppRet, arrCType, arrCType, v.ladd, cppFromElem, fallVal)
				}
			default:
				ca := append(callArgs, "&__h")
				fmt.Fprintf(&b, "\t\t\t%s __r = %s(%s);\n\t\t\tif (__h) return %s;\n\t\t}\n\t\treturn %s;\n\t}\n",
					v.cType, v.tramp, strings.Join(ca, ", "), fmt.Sprintf(v.cppCast, "__r"), fallVal)
			}
		}
		fmt.Fprint(&b, "};\n\n")
	}
	// Per-class registry of live signal connections, keyed by the callback id, so Disconnect(id) can
	// tear down exactly one connection. Mutex-guarded: connect/disconnect may be called off the main
	// thread (the emit-side lambda never touches the map, so deliveries stay lock-free).
	if len(sigs) > 0 {
		fmt.Fprintf(&b, "static std::unordered_map<long long, QMetaObject::Connection> __conns_%s;\nstatic std::mutex __connsMu_%s;\n\n", cn, cn)
	}
	fmt.Fprint(&b, "extern \"C\" {\n\n")
	if base.has {
		// static_cast does the multiple-inheritance-correct adjustment from derived* to primary-base*.
		fmt.Fprintf(&b, "void* %s(void* self) { return (void*)static_cast<%s*>((%s*)self); }\n", base.cFunc, base.cppName, cls)
	}
	if len(chain) >= 2 {
		// The whole primary-base chain in one crossing: successive static_casts perform each level's
		// (multiple-inheritance-correct) adjustment; bind<Cls> reads them back as its embedded handles.
		fmt.Fprintf(&b, "void %s_chain(void* self, void** out) {\n\t%s* __p = (%s*)self;\n", cn, cls, cls)
		prev := "__p"
		for i, link := range chain {
			fmt.Fprintf(&b, "\t%s* __b%d = static_cast<%s*>(%s); out[%d] = (void*)__b%d;\n", link.cppName, i, link.cppName, prev, i, i)
			prev = fmt.Sprintf("__b%d", i)
		}
		fmt.Fprint(&b, "}\n")
	}
	if len(virts) > 0 {
		// Registration-time mask flip (see __goOn above). Only ever called with a handle created by a
		// New<Cls> (Go guards on UnsafeGoID() != 0), so the __Go cast is sound.
		fmt.Fprintf(&b, "void %s__set_override(void* self, int idx) { ((%s__Go*)self)->__goOn[idx] = 1; }\n", cn, cn)
	}
	if isQObject && hasMeta && cn != "QObject" {
		// Checked downcast: nullptr if o isn't really a cls (qobject_cast walks the meta-object chain).
		fmt.Fprintf(&b, "void* %s_fromQObject(void* o) { return (void*)qobject_cast<%s*>((QObject*)o); }\n", cn, cls)
	}
	for _, m := range ms {
		var pre, post []string
		var args []string
		for _, p := range m.params {
			pre = append(pre, p.cppPre...)
			post = append(post, p.cppPost...) // e.g. write an output param back to its C-ABI pointer
			args = append(args, p.cppArg)
		}
		sig := cSig(!m.isStat && !m.isCtor, m)
		subCtor := m.isCtor && len(virts) > 0 // subclassable: New builds the overridable __Go subclass
		if subCtor {
			if sig == "" {
				sig = "long long __goid"
			} else {
				sig = "long long __goid, " + sig
			}
		}
		// purego packed-args shim (>15-arg methods): take a size_t array instead of N params, then
		// re-materialise each param as a local (so the body's cppArg expressions are unchanged).
		packed := purego && pgPackable(m)
		if packed {
			sig = "void* self, void* __ap"
		}
		fmt.Fprintf(&b, "%s %s(%s) {\n", retDeclCpp(m), m.cFunc, sig)
		if packed {
			b.WriteString("\tsize_t* __a = (size_t*)__ap;\n")
			idx := 0
			for _, p := range m.params {
				for _, cp := range p.cParams {
					fmt.Fprintf(&b, "\t%s = (%s)__a[%d];\n", cp, cTypeOf(cp), idx)
					idx++
				}
			}
		}
		for _, s := range pre {
			fmt.Fprintf(&b, "\t%s\n", s)
		}
		callArgs := strings.Join(args, ", ")
		emitPost := func() {
			for _, s := range post {
				fmt.Fprintf(&b, "\t%s\n", s)
			}
		}
		// The call expression: normally `recv->method(args)`; a free/friend operator uses operator syntax
		// with self as the left operand — `(*(Cls*)self) <sym> (arg)` (binary) or `<sym>(*(Cls*)self)` (unary).
		callExpr := recvCpp(cls, cn, m) + m.cppCall + "(" + callArgs + ")"
		if m.freeOpSym != "" {
			if (m.freeOpSym == "++" || m.freeOpSym == "--") && callArgs != "" {
				// POST-increment/decrement: operator++(T&, int) / operator--(T&, int). The trailing int is a
				// DUMMY overload-disambiguator, NOT an operand — emit the postfix form `(*self)++`. The binary
				// branch below would emit `(*self) ++ (arg)`, which C++ parses as `((*self)++)(arg)` -> "no
				// match for call to (T)(int&)". (Prefix ++/-- has no int param, so it takes the unary branch.)
				callExpr = "(*(" + cls + "*)self)" + m.freeOpSym
			} else if callArgs == "" {
				callExpr = m.freeOpSym + "(*(" + cls + "*)self)"
			} else {
				callExpr = "(*(" + cls + "*)self) " + m.freeOpSym + " (" + callArgs + ")"
			}
		}
		// Conversion operator: invoke via static_cast to the target type (no params).
		if m.convType != "" {
			callExpr = "static_cast<" + m.convType + ">(*(" + cls + "*)self)"
		}
		// Secondary-base accessor (gap 1): MI-correct pointer cast to the base subobject.
		if m.baseCast != "" {
			callExpr = "static_cast<" + m.baseCast + "*>((" + cls + "*)self)"
		}
		// ToQVariant (gap 2): box the value via QVariant::fromValue<T> (the by-value QVariant return is then
		// heap-copied by the record-return marshalling).
		if m.variantWrap {
			callExpr = "QVariant::fromValue(*(" + cls + "*)self)"
		}
		// Public data-member accessors: getter reads `recv->field`; setter assigns `recv->field = arg`.
		if m.fieldGet != "" {
			callExpr = recvCpp(cls, cn, m) + m.fieldGet
		}
		if m.fieldSet != "" {
			callExpr = recvCpp(cls, cn, m) + m.fieldSet + " = " + callArgs
		}
		switch {
		case subCtor:
			ca := "__goid"
			if callArgs != "" {
				ca = "__goid, " + callArgs
			}
			fmt.Fprintf(&b, "\treturn new %s__Go(%s);\n", cn, ca)
		case m.isCtor:
			fmt.Fprintf(&b, "\treturn new %s(%s);\n", cls, callArgs)
		case m.ret.isVoid:
			fmt.Fprintf(&b, "\t%s;\n", callExpr)
			emitPost()
		default:
			// A public-field getter reads the member BY VALUE: `auto&&` would bind a reference to it, which
			// is ILLEGAL for a BIT-FIELD (QWidgetData::window_state : 8, QAccessible::State flags, ...). Plain
			// `auto` copies the value out — correct for every field kind.
			capture := "auto&&"
			if m.fieldGet != "" {
				capture = "auto"
			}
			fmt.Fprintf(&b, "\t%s __r = %s;\n", capture, callExpr)
			emitPost()
			for _, s := range m.ret.cppPre {
				fmt.Fprintf(&b, "\t%s\n", s)
			}
			fmt.Fprintf(&b, "\treturn %s;\n", m.ret.cppReturn)
		}
		fmt.Fprint(&b, "}\n")
	}
	for _, s := range sigs {
		if purego { // dispatch-pointer connect shim (skips unsupported shapes, in lockstep with the Go side)
			fmt.Fprint(&b, pgConnectShimCpp(cn, cls, s))
			continue
		}
		var lambdaParams, trampArgs, pre, argTypes []string
		// The callback id is named __id (not id): a Qt signal arg is often literally named `id`
		// (QButtonGroup::idClicked(int id), ...), which would collide with a bare `id` capture/param.
		trampArgs = append(trampArgs, "__id")
		for i, a := range s.args {
			// The lambda declares EVERY arg (so &Class::member / the static_cast signature matches), but
			// forwards only the first `exposed` to the trampoline — the trailing default-valued args are
			// accepted and ignored (the func() variant of clicked(bool=false)).
			lambdaParams = append(lambdaParams, a.cppType+" "+a.name)
			argTypes = append(argTypes, a.cppType)
			if i < s.exposed {
				pre = append(pre, a.cppPre...)
				trampArgs = append(trampArgs, a.cppArgs...)
			}
		}
		// An overloaded signal name needs the member pointer cast to the exact overload signature, or
		// &Class::member is ambiguous (signals always return void).
		memberPtr := fmt.Sprintf("&%s::%s", cls, s.member)
		if s.overloaded {
			memberPtr = fmt.Sprintf("static_cast<void(%s::*)(%s)>(&%s::%s)", cls, strings.Join(argTypes, ", "), cls, s.member)
		}
		fmt.Fprintf(&b, "void %s(void* self, long long __id) {\n", s.connectC)
		fmt.Fprintf(&b, "\tQMetaObject::Connection __c = QObject::connect((%s*)self, %s, (%s*)self, [__id](%s){\n",
			cls, memberPtr, cls, strings.Join(lambdaParams, ", "))
		for _, p := range pre {
			fmt.Fprintf(&b, "\t\t%s\n", p)
		}
		fmt.Fprintf(&b, "\t\t%s(%s);\n\t});\n", s.tramp, strings.Join(trampArgs, ", "))
		fmt.Fprintf(&b, "\tstd::lock_guard<std::mutex> __lk(__connsMu_%s);\n\t__conns_%s[__id] = __c;\n}\n", cn, cn)
	}
	if len(sigs) > 0 {
		// Take the Connection out under the lock, disconnect outside it (disconnect can synchronously
		// touch QObject internals; holding our mutex across it invites lock-order surprises).
		fmt.Fprintf(&b, "void %s__disconnect(long long id) {\n\tQMetaObject::Connection __c;\n\t{\n\t\tstd::lock_guard<std::mutex> __lk(__connsMu_%s);\n\t\tauto __it = __conns_%s.find(id);\n\t\tif (__it == __conns_%s.end()) return;\n\t\t__c = __it->second;\n\t\t__conns_%s.erase(__it);\n\t}\n\tQObject::disconnect(__c);\n}\n", cn, cn, cn, cn, cn)
	}
	if len(virts) > 0 {
		if isAppClass(cn) {
			// (int& argc, char** argv) ctor via the variadic subclass ctor.
			fmt.Fprintf(&b, "void* %s_new_args(long long __goid, int* argc, char** argv) { return new %s__Go(__goid, *argc, argv); }\n", cn, cn)
		} else if parentCtor {
			fmt.Fprintf(&b, "void* %s__Go_new(long long id, void* parent) { return new %s__Go(id, (QObject*)parent); }\n", cn, cn)
		} else {
			fmt.Fprintf(&b, "void* %s__Go_new(long long id) { return new %s__Go(id); }\n", cn, cn)
		}
	}
	// App classes whose only ctor is (int& argc, char** argv) aren't subclassable (no parent/default ctor),
	// so the virts block above never emits their constructor — emit a plain one so New<App> exists.
	if len(virts) == 0 && isAppClass(cn) {
		fmt.Fprintf(&b, "void* %s_new_args(int* argc, char** argv) { return new %s(*argc, argv); }\n", cn, cn)
	}
	// A non-public/deleted destructor can't be `delete`d from here; skip the shim (the Go Delete()
	// is correspondingly a no-op for these — a small leak in exchange for a compilable binding).
	if publicDtor {
		fmt.Fprintf(&b, "void %s_delete(void* self) { delete (%s*)self; }\n", cn, cls)
	}
	fmt.Fprint(&b, "\n}\n")
	return b.String()
}

func recvCpp(cls, cn string, m method) string {
	if m.isStat {
		return cls + "::"
	}
	if m.protCall || m.subclassRecv {
		// Protected method / protected-virtual Default: reach it through the generated subclass (the
		// subclass has protected access). protCall calls it via a `using` decl; subclassRecv calls the
		// base qualified (cppCall is already Cls::member), so no `using` is needed.
		return fmt.Sprintf("((%s__Go*)self)->", cn)
	}
	return fmt.Sprintf("((%s*)self)->", cls)
}

func retDeclCpp(m method) string {
	if m.isCtor {
		return "void*"
	}
	return m.ret.cRetType
}

func emitGo(pkg, cn string, ms []method, sigs []signal, virts []vmethod, isQObject, hasMeta, parentCtor, parentOpt, publicDtor bool, qobjType, baseImport string, base baseEmit, chain []baseEmit) string {
	var b strings.Builder
	// Struct: a derived class EMBEDS its primary base (miqt-style) and inherits the base's methods via
	// Go promotion — they are not re-emitted. Each level stores its OWN base-cast handle (the <cn>_base
	// helper performs the cast), so a method runs on the correctly-adjusted pointer. A root (no embeddable
	// base) carries the handle plus the per-instance virtual-override id, reached from any level via the
	// promoted, exported Unsafe{,Set}GoID accessors (so cross-package subclasses can use it).
	if base.has {
		fmt.Fprintf(&b, "type %s struct {\n\th unsafe.Pointer\n\t*%s\n}\n\n", cn, base.goType)
	} else {
		fmt.Fprintf(&b, "type %s struct {\n\th   unsafe.Pointer\n\tvid int64 // virtual-override id (0 for non-subclass instances)\n}\n\n", cn)
		fmt.Fprintf(&b, "func (this *%s) UnsafeGoID() int64     { return this.vid }\n", cn)
		fmt.Fprintf(&b, "func (this *%s) UnsafeSetGoID(id int64) { this.vid = id }\n\n", cn)
	}
	// bind wraps a handle (no finalizer — borrowing). For a derived class it builds the embedded base
	// wrappers from the base-subobject pointers; the bases are built borrowed (they share this object's
	// lifetime). A 2+-level same-package chain fetches ALL its base pointers in ONE <cn>_chain crossing
	// (instead of one <cn>_base call per level). newowned adds the GC finalizer for owned value types.
	switch {
	case len(chain) >= 2:
		k := len(chain)
		last := chain[k-1]
		var inner string
		if last.cross {
			// The chain crosses into the base package here; its own (chain-optimized) bind continues.
			inner = fmt.Sprintf("%sUnsafeNew%s(__c[%d])", last.pfx, last.bare, k-1)
		} else {
			inner = fmt.Sprintf("&%s{h: __c[%d]}", last.bare, k-1)
		}
		for i := k - 2; i >= 0; i-- {
			inner = fmt.Sprintf("&%s{h: __c[%d], %s: %s}", chain[i].bare, i, chain[i+1].field, inner)
		}
		fmt.Fprintf(&b, "func bind%s(h unsafe.Pointer) *%s {\n\tif h == nil {\n\t\treturn nil\n\t}\n\tvar __c [%d]unsafe.Pointer\n\tC.%s_chain(h, &__c[0])\n\treturn &%s{h: h, %s: %s}\n}\n\n",
			cn, cn, k, cn, cn, chain[0].field, inner)
	case base.has && base.cross:
		fmt.Fprintf(&b, "func bind%s(h unsafe.Pointer) *%s {\n\tif h == nil {\n\t\treturn nil\n\t}\n\treturn &%s{h: h, %s: %sUnsafeNew%s(unsafe.Pointer(C.%s(h)))}\n}\n\n",
			cn, cn, cn, base.field, base.pfx, base.bare, base.cFunc)
	case base.has:
		fmt.Fprintf(&b, "func bind%s(h unsafe.Pointer) *%s {\n\tif h == nil {\n\t\treturn nil\n\t}\n\treturn &%s{h: h, %s: bind%s(unsafe.Pointer(C.%s(h)))}\n}\n\n",
			cn, cn, cn, base.field, base.bare, base.cFunc)
	default:
		fmt.Fprintf(&b, "func bind%s(h unsafe.Pointer) *%s {\n\tif h == nil {\n\t\treturn nil\n\t}\n\treturn &%s{h: h}\n}\n\n", cn, cn, cn)
	}
	// newowned wraps a Go-owned handle. A value type (public dtor, not QObject) gets a GC finalizer so an
	// un-Delete()d wrapper is still freed when unreachable. QObject lifetimes are Qt-managed (parent/child,
	// deleteLater) and non-public-dtor types can't be freed by us — neither gets a finalizer.
	if isQObject || !publicDtor {
		fmt.Fprintf(&b, "func newowned%s(h unsafe.Pointer) *%s {\n\treturn bind%s(h)\n}\n\n", cn, cn, cn)
	} else {
		fmt.Fprintf(&b, "func newowned%s(h unsafe.Pointer) *%s {\n\to := bind%s(h)\n\tif o != nil {\n\t\truntime.SetFinalizer(o, (*%s).Delete)\n\t}\n\treturn o\n}\n\n", cn, cn, cn, cn)
	}
	// chandle yields the C handle, mapping a nil Go wrapper to nullptr (a nil arg crosses as C++ nullptr).
	fmt.Fprintf(&b, "func (this *%s) chandle() unsafe.Pointer {\n\tif this == nil {\n\t\treturn nil\n\t}\n\treturn this.h\n}\n\n", cn)
	// Cross-package protocol: a dependent package (e.g. kf6 referencing a qt.QWidget) can't touch the
	// unexported h field, so it uses these exported helpers. UnsafePointer reads the (most-derived) handle;
	// UnsafeNew<T> borrows; UnsafeNew<T>Owned owns.
	fmt.Fprintf(&b, "func (this *%s) UnsafePointer() unsafe.Pointer { return this.chandle() }\n", cn)
	fmt.Fprintf(&b, "func UnsafeNew%s(h unsafe.Pointer) *%s { return bind%s(h) }\n", cn, cn, cn)
	fmt.Fprintf(&b, "func UnsafeNew%sOwned(h unsafe.Pointer) *%s { return newowned%s(h) }\n\n", cn, cn, cn)
	// UnsafeRelease drops this wrapper's handle AND every embedded base wrapper's handle without
	// freeing the C++ object. Delete calls it after freeing, so a post-Delete call through a PROMOTED
	// base method hits a nil handle (crossing as C++ nullptr) instead of a use-after-free — previously
	// only the most-derived handle was nil'd and the embedded bases kept live pointers to freed memory.
	if base.has {
		fmt.Fprintf(&b, "func (this *%s) UnsafeRelease() {\n\tthis.h = nil\n\tif this.%s != nil {\n\t\tthis.%s.UnsafeRelease()\n\t}\n}\n\n", cn, base.field, base.field)
	} else {
		fmt.Fprintf(&b, "func (this *%s) UnsafeRelease() { this.h = nil }\n\n", cn)
	}
	// Cast<T> is a CHECKED downcast (qobject_cast): given any QObject, return *T if the object really is a
	// T (incl. a subclass), else nil — the safe alternative to UnsafeNew<T>, e.g. inspecting Children().
	// QObject-only AND the target must declare Q_OBJECT (its own staticMetaObject) — qobject_cast<T*>
	// static_asserts otherwise (e.g. KViewStateMaintainerBase derives QObject but omits Q_OBJECT). nil-safe.
	if isQObject && hasMeta && cn != "QObject" {
		fmt.Fprintf(&b, "func Cast%s(o %s) *%s {\n\treturn UnsafeNew%s(C.%s_fromQObject(o.UnsafePointer()))\n}\n\n", cn, qobjType, cn, cn, cn)
	}

	for _, m := range ms {
		var goParams, goPre, goArgs, goPost, outTypes, outExprs []string
		for _, p := range m.params {
			if p.outRetType != "" { // an output value -> an extra Go return
				outTypes = append(outTypes, p.outRetType)
				outExprs = append(outExprs, p.outRetExpr)
			}
			if p.goParam != "" { // an input value (an in/out ref param has BOTH goParam and outRetType)
				goParams = append(goParams, p.goParam)
			}
			goPre = append(goPre, p.goPre...)
			goArgs = append(goArgs, p.goArgs...)
			goPost = append(goPost, p.goPost...) // e.g. free C strings built for a []string param
			for _, ka := range p.keepAlive {     // pin wrapper args (finalizer UAF) + borrowed buffers across the call
				goPost = append(goPost, "runtime.KeepAlive("+ka+")")
			}
		}
		if !m.isStat && !m.isCtor {
			// Pin the receiver: the wrapper itself is unreferenced once this.h is read, so without this
			// its GC finalizer could free the C++ object while the shim call is still executing.
			goPost = append(goPost, "runtime.KeepAlive(this)")
		}
		goPre = append(goPre, m.ret.goPre...)
		callArgs := append([]string{}, goArgs...)
		callArgs = append(callArgs, m.ret.cgoExtra...)

		sig := strings.Join(goParams, ", ")
		switch {
		case m.isCtor && len(virts) > 0:
			// Subclassable: every New<Cls> builds the overridable __Go subclass with a fresh override id,
			// so On<Virtual>/<Virtual>Default work on any constructed instance (universal overridability).
			fmt.Fprintf(&b, "func %s(%s) *%s {\n", m.goName, sig, cn)
			emitGoPre(&b, goPre)
			cargs := append([]string{"C.longlong(__goid)"}, callArgs...)
			fmt.Fprintf(&b, "\t__goid := nextVirtID()\n\t__o := newowned%s(C.%s(%s))\n", cn, m.cFunc, strings.Join(cargs, ", "))
			emitGoPre(&b, goPost)
			fmt.Fprintf(&b, "\t__o.UnsafeSetGoID(__goid)\n\tregisterInstance(__goid, __o.h, %s_virtIdx, %s_enable)\n\treturn __o\n}\n\n", cn, cn)
		case m.isCtor:
			fmt.Fprintf(&b, "func %s(%s) *%s {\n", m.goName, sig, cn)
			emitGoPre(&b, goPre)
			fmt.Fprintf(&b, "\t__o := newowned%s(C.%s(%s))\n", cn, m.cFunc, strings.Join(callArgs, ", "))
			emitGoPre(&b, goPost)
			fmt.Fprint(&b, "\treturn __o\n}\n\n")
		default:
			recv := ""
			selfArg := ""
			if !m.isStat {
				recv = fmt.Sprintf("(this *%s) ", cn)
				selfArg = "this.h"
			}
			ca := append([]string{}, callArgs...)
			if selfArg != "" {
				ca = append([]string{selfArg}, ca...)
			}
			// Return signature = the main return (if any) + each output param, parenthesised when >1.
			var allTypes []string
			if !m.ret.isVoid && m.ret.goRetType != "" {
				allTypes = append(allTypes, m.ret.goRetType)
			}
			allTypes = append(allTypes, outTypes...)
			retDecl := ""
			if len(allTypes) == 1 {
				retDecl = " " + allTypes[0]
			} else if len(allTypes) > 1 {
				retDecl = " (" + strings.Join(allTypes, ", ") + ")"
			}
			fmt.Fprintf(&b, "func %s%s(%s)%s {\n", recv, m.goName, sig, retDecl)
			emitGoPre(&b, goPre)
			shimName := registerShimShape(!m.isStat, m, "gen_"+strings.ToLower(cn)+".h")
			shimCa := append([]string{"C." + m.cFunc}, ca...)
			call := fmt.Sprintf("C.%s(%s)", shimName, strings.Join(shimCa, ", "))
			if m.ret.isVoid {
				fmt.Fprintf(&b, "\t%s\n", call)
				emitGoPre(&b, goPost)
				if len(outExprs) > 0 {
					fmt.Fprintf(&b, "\treturn %s\n", strings.Join(outExprs, ", "))
				}
			} else {
				fmt.Fprintf(&b, "\t__r := %s\n", call)
				emitGoPre(&b, goPost)
				emitGoPre(&b, m.ret.goPost)
				fmt.Fprintf(&b, "\treturn %s\n", strings.Join(append([]string{m.ret.goReturn}, outExprs...), ", "))
			}
			fmt.Fprint(&b, "}\n\n")
		}
	}
	// Signals: a Connect<Sig> method registers the Go callback (by integer id, since a Go func
	// can't cross into C) and a //export trampoline the C++ connect-lambda calls on each emit.
	for _, s := range sigs {
		var goTypes, cgoParams, goConvs []string
		for _, a := range s.args[:s.exposed] { // only the surfaced (non-dropped) args reach Go
			goTypes = append(goTypes, a.goType)
			cgoParams = append(cgoParams, a.cgoParams...)
			goConvs = append(goConvs, a.goConv)
		}
		fnType := "func(" + strings.Join(goTypes, ", ") + ")"
		// Returns a connection handle; pass it to Disconnect to tear down this one connection.
		fmt.Fprintf(&b, "func (this *%s) %s(fn %s) int64 {\n\tid := registerCallback(fn)\n\tC.%s(this.h, C.longlong(id))\n\truntime.KeepAlive(this)\n\treturn id\n}\n\n",
			cn, s.connectGo, fnType, s.connectC)
		trampParams := append([]string{"__id C.longlong"}, cgoParams...)
		fmt.Fprintf(&b, "//export %s\nfunc %s(%s) {\n", s.tramp, s.tramp, strings.Join(trampParams, ", "))
		fmt.Fprintf(&b, "\tif __fn, __ok := lookupCallback(int64(__id)).(%s); __ok {\n\t\t__fn(%s)\n\t}\n}\n\n",
			fnType, strings.Join(goConvs, ", "))
	}

	if len(sigs) > 0 {
		// Tear down the Qt connection, then release the registered Go closure — without the unregister,
		// connect/disconnect-heavy long-running apps leaked every callback ever registered.
		fmt.Fprintf(&b, "func (this *%s) Disconnect(handle int64) {\n\tC.%s__disconnect(C.longlong(handle))\n\tunregisterCallback(handle)\n}\n\n", cn, cn)
	}

	// Virtual overrides. On<Virtual>(fn) registers a Go impl for an instance; <Virtual>Default(args)
	// calls the base implementation (the clean alternative to miqt's inlined super parameter); the
	// //export trampoline runs the override (reporting "handled" so C++ knows whether to fall through).
	// Universal overridability: a regular New<Cls> already builds the overridable subclass. Only an
	// ABSTRACT class (no public ctor) needs a dedicated constructor here — exposed as New<Cls> too.
	hasCtor := false
	for _, m := range ms {
		if m.isCtor {
			hasCtor = true
			break
		}
	}
	if len(virts) > 0 {
		// Per-class mask plumbing: <cn>_virtIdx maps a global slot id to this class's __Go override-mask
		// index; <cn>_enable flips that bit C++-side. Both are handed to registerInstance at construction
		// so a later On<X> — even one called through a PROMOTED base wrapper — enables the right bit of
		// the right (most-derived) subclass.
		fmt.Fprintf(&b, "func %s_virtIdx(slot int64) int32 {\n\tswitch slot {\n", cn)
		for _, v := range virts {
			fmt.Fprintf(&b, "\tcase %d:\n\t\treturn %d\n", v.slotID, v.idx)
		}
		fmt.Fprint(&b, "\t}\n\treturn -1\n}\n\n")
		fmt.Fprintf(&b, "func %s_enable(h unsafe.Pointer, idx int32) { C.%s__set_override(h, C.int(idx)) }\n\n", cn, cn)
		if isAppClass(cn) {
			// New<Cls>(args []string): long-lived argc/argv (never freed, as Qt keeps them), main-thread lock.
			fmt.Fprintf(&b, "func New%s(args []string) *%s {\n\targc := (*C.int)(C.malloc(C.size_t(unsafe.Sizeof(C.int(0)))))\n\t*argc = C.int(len(args))\n\targv := (*[1 << 16]*C.char)(C.malloc(C.size_t(uintptr(len(args)+1) * unsafe.Sizeof((*C.char)(nil)))))\n\tfor i := range args {\n\t\targv[i] = C.CString(args[i])\n\t}\n\targv[len(args)] = nil\n\truntime.LockOSThread()\n\t__goid := nextVirtID()\n\to := newowned%s(C.%s_new_args(C.longlong(__goid), argc, &argv[0]))\n\to.UnsafeSetGoID(__goid)\n\tregisterInstance(__goid, o.h, %s_virtIdx, %s_enable)\n\treturn o\n}\n\n", cn, cn, cn, cn, cn, cn)
		} else if !hasCtor {
			if parentCtor && parentOpt {
				// Parent is defaulted (Cls(QObject* parent=nullptr)) -> miqt-style pair: no-arg New<Cls>()
				// (parent=nullptr) keeps the base name, New<Cls>2(parent) takes an explicit parent.
				fmt.Fprintf(&b, "func New%s() *%s {\n\tid := nextVirtID()\n\to := newowned%s(C.%s__Go_new(C.longlong(id), nil))\n\to.UnsafeSetGoID(id)\n\tregisterInstance(id, o.h, %s_virtIdx, %s_enable)\n\treturn o\n}\n\n", cn, cn, cn, cn, cn, cn)
				fmt.Fprintf(&b, "func New%s2(parent %s) *%s {\n\tid := nextVirtID()\n\to := newowned%s(C.%s__Go_new(C.longlong(id), parent.UnsafePointer()))\n\to.UnsafeSetGoID(id)\n\tregisterInstance(id, o.h, %s_virtIdx, %s_enable)\n\treturn o\n}\n\n", cn, qobjType, cn, cn, cn, cn, cn)
			} else if parentCtor {
				fmt.Fprintf(&b, "func New%s(parent %s) *%s {\n\tid := nextVirtID()\n\to := newowned%s(C.%s__Go_new(C.longlong(id), parent.UnsafePointer()))\n\to.UnsafeSetGoID(id)\n\tregisterInstance(id, o.h, %s_virtIdx, %s_enable)\n\treturn o\n}\n\n", cn, qobjType, cn, cn, cn, cn, cn)
			} else {
				fmt.Fprintf(&b, "func New%s() *%s {\n\tid := nextVirtID()\n\to := newowned%s(C.%s__Go_new(C.longlong(id)))\n\to.UnsafeSetGoID(id)\n\tregisterInstance(id, o.h, %s_virtIdx, %s_enable)\n\treturn o\n}\n\n", cn, cn, cn, cn, cn, cn)
			}
		}
		for _, v := range virts {
			var goTypes, cgoParams, goConvs []string
			for _, a := range v.args {
				goTypes = append(goTypes, a.goType)
				cgoParams = append(cgoParams, a.cgoParams...)
				goConvs = append(goConvs, a.goConv)
			}
			fnType := "func(" + strings.Join(goTypes, ", ") + ")"
			if !v.isVoid {
				fnType += " " + v.goRet
			}
			// slot ids are generator-computed hashes of the slot key (see slotHash) — no strings cross.
			fmt.Fprintf(&b, "func (this *%s) %s(fn %s) {\n\tregisterVirt(this.UnsafeGoID(), %d, fn) // slot %q\n}\n\n",
				cn, v.onGo, fnType, v.slotID, v.slotKey)
			base := append([]string{"__id C.longlong"}, cgoParams...) // __id/__fn/__ok: a Qt arg may be named id/fn/ok
			call := fmt.Sprintf("__fn(%s)", strings.Join(goConvs, ", "))
			switch {
			case v.isVoid:
				trampParams := append(base, "handled *C.int")
				fmt.Fprintf(&b, "//export %s\nfunc %s(%s) {\n\tif __fn, __ok := lookupVirt(int64(__id), %d).(%s); __ok {\n\t\t*handled = 1\n\t\t%s\n\t\treturn\n\t}\n\t*handled = 0\n}\n\n",
					v.tramp, v.tramp, strings.Join(trampParams, ", "), v.slotID, fnType, call)
			case v.retQString:
				trampParams := append(base, "outlen *C.longlong", "handled *C.int")
				fmt.Fprintf(&b, "//export %s\nfunc %s(%s) *C.char {\n\tif __fn, __ok := lookupVirt(int64(__id), %d).(%s); __ok {\n\t\t*handled = 1\n\t\t__s := %s\n\t\t*outlen = C.longlong(len(__s))\n\t\treturn C.CString(__s)\n\t}\n\t*handled = 0\n\treturn nil\n}\n\n",
					v.tramp, v.tramp, strings.Join(trampParams, ", "), v.slotID, fnType, call)
			case v.retList:
				trampParams := append(base, "outlen *C.longlong", "handled *C.int")
				cgoElem, sizeExpr, setElem := v.lel.cArrGo, "", ""
				if v.lstr { // []string -> char* array (each C.CString freed C++-side)
					cgoElem, sizeExpr, setElem = "*C.char", "unsafe.Sizeof(uintptr(0))", "C.CString(__e)"
				} else {
					sizeExpr = "unsafe.Sizeof(" + cgoElem + "(0))"
					if cgoElem == "unsafe.Pointer" {
						sizeExpr = "unsafe.Sizeof(uintptr(0))"
					}
					setElem = fmt.Sprintf(v.lel.goToC, "__e")
				}
				fmt.Fprintf(&b, "//export %s\nfunc %s(%s) unsafe.Pointer {\n\tif __fn, __ok := lookupVirt(int64(__id), %d).(%s); __ok {\n\t\t*handled = 1\n\t\t__rs := %s\n\t\t*outlen = C.longlong(len(__rs))\n\t\tif len(__rs) == 0 {\n\t\t\treturn nil\n\t\t}\n\t\t__a := C.malloc(C.size_t(len(__rs)) * C.size_t(%s))\n\t\t__arr := (*[1 << 28]%s)(__a)\n\t\tfor __i, __e := range __rs {\n\t\t\t__arr[__i] = %s\n\t\t}\n\t\treturn __a\n\t}\n\t*handled = 0\n\treturn nil\n}\n\n",
					v.tramp, v.tramp, strings.Join(trampParams, ", "), v.slotID, fnType, call, sizeExpr, cgoElem, setElem)
			default:
				trampParams := append(base, "handled *C.int")
				fmt.Fprintf(&b, "//export %s\nfunc %s(%s) %s {\n\tif __fn, __ok := lookupVirt(int64(__id), %d).(%s); __ok {\n\t\t*handled = 1\n\t\treturn %s\n\t}\n\t*handled = 0\n\treturn %s\n}\n\n",
					v.tramp, v.tramp, strings.Join(trampParams, ", "), v.cgoRet, v.slotID, fnType, fmt.Sprintf(v.goToC, call), v.retZero)
			}
		}
	}
	// Non-subclassable app class (QApplication, ...): emit the plain New<App>(args) the virts block skipped.
	if len(virts) == 0 && isAppClass(cn) {
		fmt.Fprintf(&b, "func New%s(args []string) *%s {\n\targc := (*C.int)(C.malloc(C.size_t(unsafe.Sizeof(C.int(0)))))\n\t*argc = C.int(len(args))\n\targv := (*[1 << 16]*C.char)(C.malloc(C.size_t(uintptr(len(args)+1) * unsafe.Sizeof((*C.char)(nil)))))\n\tfor i := range args {\n\t\targv[i] = C.CString(args[i])\n\t}\n\targv[len(args)] = nil\n\truntime.LockOSThread()\n\treturn newowned%s(C.%s_new_args(argc, &argv[0]))\n}\n\n", cn, cn, cn, cn)
	}

	// Delete frees the object now and cancels the finalizer (so it can't double-free). Idempotent:
	// after Delete every handle in the wrapper chain is nil (UnsafeRelease) and delete on null is a C++
	// no-op — previously the embedded base wrappers kept live pointers to the freed object, so a
	// post-Delete promoted-method call was a use-after-free. Registered virtual overrides are dropped
	// from the registry (no leak per created-and-deleted instance). A non-public-dtor type has no
	// _delete shim, so Delete only drops the handles. (Like miqt, never Delete a borrowed wrapper.)
	del := ""
	if publicDtor {
		del = fmt.Sprintf("\tC.%s_delete(this.h)\n", cn)
	}
	fmt.Fprintf(&b, "func (this *%s) Delete() {\n\truntime.SetFinalizer(this, nil)\n\tif __id := this.UnsafeGoID(); __id != 0 {\n\t\tunregisterInstance(__id)\n\t}\n%s\tthis.UnsafeRelease()\n}\n", cn, del)

	// Assemble the file header now that the body is known: a dependent package (baseImport set) imports
	// the base "qt" package only when this file actually references a qt.* type — Go errors on an unused
	// import. The #cgo directives live once in gen_support.go, not per shim (avoids an oversized command
	// line); each shim just pulls in its own C header.
	imports := "\t\"runtime\"\n\t\"unsafe\"\n"
	if baseImport != "" && strings.Contains(b.String(), "qt.") {
		imports += "\tqt \"" + baseImport + "\"\n"
	}
	var h strings.Builder
	fmt.Fprintf(&h, "package %s\n\n", pkg)
	fmt.Fprintf(&h, "/*\n#include <stdlib.h>\n#include \"gen_%s.h\"\n#include \"cppgo_shims.h\"\n*/\nimport \"C\"\nimport (\n%s)\n\n", strings.ToLower(cn), imports)
	return h.String() + b.String()
}

func emitGoPre(b *strings.Builder, stmts []string) {
	for _, s := range stmts {
		fmt.Fprintf(b, "\t%s\n", s)
	}
}

func retDeclGo(m method) string {
	if m.ret.isVoid || m.ret.goRetType == "" {
		return ""
	}
	return " " + m.ret.goRetType
}

func referencedHeaders(r ir.Record, rv Resolver) []string {
	seen := map[string]bool{}
	var out []string
	add := func(h string) {
		if h != "" && h != r.Header && !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	var visit func(t ir.Type)
	visit = func(t ir.Type) {
		switch t.Kind {
		case "record":
			if rv.Bound[t.USR] {
				add(rv.Header[t.USR])
			}
		case "enum":
			add(rv.EnumHeader[t.USR])
		case "pointer", "ref":
			if t.Elem != nil {
				visit(*t.Elem)
			}
		case "template":
			for i := range t.Args {
				visit(t.Args[i])
			}
		case "typedef":
			if t.Canonical != nil {
				visit(*t.Canonical)
			}
		}
	}
	for _, b := range r.Bases {
		if rv.Bound[b.USR] {
			add(rv.Header[b.USR])
		}
	}
	for _, ms := range [][]ir.Method{r.Ctors, r.Methods, r.PureVirtuals} {
		for _, m := range ms {
			visit(m.Ret)
			for _, p := range m.Params {
				visit(p.Type)
			}
		}
	}
	for _, fld := range r.Fields { // public-field getter/setter shims name the field type
		visit(fld.Type)
	}
	// Free operators bound onto this record reference their own arg/return types in the shim.
	for _, f := range rv.FreeOps[r.USR] {
		visit(f.Ret)
		for _, p := range f.Params {
			visit(p.Type)
		}
	}
	return out
}

// EmitFunctions emits package-level Go funcs (+ a C-ABI header and C++ shim) for the free, NON-operator
// functions passed in (already filtered to package `pkg`, e.g. qFuzzyCompare). Free OPERATORS are bound
// as methods on their first record argument (EmitClass), not here. Returns "","","" if nothing to emit.
func EmitFunctions(funcs []ir.Func, pkg string, rv Resolver) (goSrc, hSrc, cppSrc string) {
	cUsed, goUsed := map[string]int{}, map[string]int{}
	// Seed goUsed with THIS package's record type names so a free function whose exported name collides
	// with a same-package type (qRgba64 -> QRgba64, which is also the QRgba64 struct) is disambiguated by
	// overloadName (-> QRgba64WithX / QRgba642) instead of producing a Go "redeclared in this block" error.
	for usr, nm := range rv.Name {
		if rv.Bound[usr] && rv.Pkg[usr] == rv.Self {
			goUsed[strings.ReplaceAll(nm, "::", "__")]++
		}
	}
	uniq := func(m map[string]int, base string) string {
		m[base]++
		if m[base] == 1 {
			return base
		}
		return fmt.Sprintf("%s%d", base, m[base])
	}
	hdrSet := map[string]bool{}
	var visit func(t ir.Type)
	visit = func(t ir.Type) {
		switch t.Kind {
		case "record":
			if rv.Bound[t.USR] && rv.Header[t.USR] != "" {
				hdrSet[rv.Header[t.USR]] = true
			}
		case "enum":
			if rv.EnumHeader[t.USR] != "" {
				hdrSet[rv.EnumHeader[t.USR]] = true
			}
		case "pointer", "ref":
			if t.Elem != nil {
				visit(*t.Elem)
			}
		case "template":
			for i := range t.Args {
				visit(t.Args[i])
			}
		case "typedef":
			if t.Canonical != nil {
				visit(*t.Canonical)
			}
		}
	}
	type item struct {
		mm   method
		call string // fully-qualified C++ call spelling, e.g. "qFuzzyCompare" / "Qt::foo"
	}
	var items []item
	for _, f := range funcs {
		if strings.HasPrefix(f.Name, "operator") {
			continue // operators are attached to their first record arg (EmitClass)
		}
		fm := ir.Method{Name: f.Name, Static: true, Ret: f.Ret, Params: f.Params}
		mm, ok := plan(fm, false, rv)
		if !ok {
			continue
		}
		mm.isStat = true
		// Package-qualify the free-function C symbol: two packages linked into ONE executable (qt + kf6)
		// each have e.g. a free `reset` -> both would emit `cppgofn_reset` -> "multiple definition" at the
		// executable link. (Per-class method symbols are already unique via the class name.) pkg is a valid
		// C identifier for cppgo's packages (qt/kf6/webengine/pdf).
		mm.cFunc = uniq(cUsed, "cppgofn_"+pkg+"_"+f.Name)
		mm.goName = overloadName(exported(f.Name), goUsed, fm)
		items = append(items, item{mm, f.CppName})
		if f.Header != "" {
			hdrSet[f.Header] = true
		}
		visit(f.Ret)
		for _, p := range f.Params {
			visit(p.Type)
		}
	}
	if len(items) == 0 {
		return "", "", ""
	}
	purego := rv.Backend == "purego"
	var gb, hb, cb strings.Builder
	var pgVars, pgRegs []string
	for _, it := range items {
		mm := it.mm
		if purego {
			// purego free function: a static method (no receiver) — direct SyscallN when the signature is
			// register-only, else RegisterLibFunc. (Packed-args shims aren't emitted for free functions —
			// emitFuncCpp uses the normal signature — so a >15-arg free function stays unbound; none in qt.)
			if d, vn, vs, sym, direct, ok := pgMethod("", mm, false); ok && !pgPackable(mm) {
				gb.WriteString(d)
				if direct {
					pgVars = append(pgVars, "\t"+vn+" uintptr")
					pgRegs = append(pgRegs, fmt.Sprintf("\tsymaddr(&%s, %q)", vn, sym))
				} else {
					pgVars = append(pgVars, "\t"+vn+" "+vs)
					pgRegs = append(pgRegs, fmt.Sprintf("\treg(&%s, %q)", vn, sym))
				}
			}
			emitFuncCpp(&hb, &cb, it.mm, it.call)
			continue
		}
		var goParams, goPre, goArgs, goPost, outTypes, outExprs []string
		for _, p := range mm.params {
			if p.outRetType != "" { // an output value -> an extra Go return
				outTypes = append(outTypes, p.outRetType)
				outExprs = append(outExprs, p.outRetExpr)
			}
			if p.goParam != "" { // an input value (an in/out ref param has BOTH)
				goParams = append(goParams, p.goParam)
			}
			goPre = append(goPre, p.goPre...)
			goArgs = append(goArgs, p.goArgs...)
			goPost = append(goPost, p.goPost...)
			for _, ka := range p.keepAlive { // pin wrapper args / borrowed buffers across the call
				goPost = append(goPost, "runtime.KeepAlive("+ka+")")
			}
		}
		goPre = append(goPre, mm.ret.goPre...)
		callArgs := append([]string{}, goArgs...)
		callArgs = append(callArgs, mm.ret.cgoExtra...)
		var allTypes []string
		if !mm.ret.isVoid && mm.ret.goRetType != "" {
			allTypes = append(allTypes, mm.ret.goRetType)
		}
		allTypes = append(allTypes, outTypes...)
		retDecl := ""
		if len(allTypes) == 1 {
			retDecl = " " + allTypes[0]
		} else if len(allTypes) > 1 {
			retDecl = " (" + strings.Join(allTypes, ", ") + ")"
		}
		fmt.Fprintf(&gb, "func %s(%s)%s {\n", mm.goName, strings.Join(goParams, ", "), retDecl)
		emitGoPre(&gb, goPre)
		shimName := registerShimShape(false, mm, "gen_functions.h")
		shimArgs := append([]string{"C." + mm.cFunc}, callArgs...)
		call := fmt.Sprintf("C.%s(%s)", shimName, strings.Join(shimArgs, ", "))
		if mm.ret.isVoid {
			fmt.Fprintf(&gb, "\t%s\n", call)
			emitGoPre(&gb, goPost)
			if len(outExprs) > 0 {
				fmt.Fprintf(&gb, "\treturn %s\n", strings.Join(outExprs, ", "))
			}
		} else {
			fmt.Fprintf(&gb, "\t__r := %s\n", call)
			emitGoPre(&gb, goPost)
			emitGoPre(&gb, mm.ret.goPost)
			fmt.Fprintf(&gb, "\treturn %s\n", strings.Join(append([]string{mm.ret.goReturn}, outExprs...), ", "))
		}
		fmt.Fprint(&gb, "}\n\n")
		emitFuncCpp(&hb, &cb, mm, it.call)
	}
	var hdrs []string
	for h := range hdrSet {
		hdrs = append(hdrs, h)
	}
	sort.Strings(hdrs)

	var h strings.Builder
	h.WriteString("#pragma once\n#include <stddef.h>\n#ifdef __cplusplus\nextern \"C\" {\n#endif\n\n")
	h.WriteString(hb.String())
	h.WriteString("\n#ifdef __cplusplus\n}\n#endif\n")

	var c strings.Builder
	for _, hh := range hdrs {
		fmt.Fprintf(&c, "#include %s\n", includeSpelling(hh))
	}
	c.WriteString("#include \"gen_functions.h\"\n\nextern \"C\" {\n\n")
	c.WriteString(cb.String())
	c.WriteString("\n}\n")

	body := gb.String()
	if purego {
		// purego free-functions file: pure Go (no cgo). The reg()/symaddr/_load helpers live in gen_purego.go.
		imports := ""
		if strings.Contains(body, "unsafe.") {
			imports += "\t\"unsafe\"\n"
		}
		if strings.Contains(body, "runtime.") {
			imports += "\t\"runtime\"\n"
		}
		if strings.Contains(body, "purego.") {
			imports += "\t\"github.com/ebitengine/purego\"\n"
		}
		if rv.BaseImport != "" && strings.Contains(body, "qt.") {
			imports += "\tqt \"" + rv.BaseImport + "\"\n"
		}
		var g strings.Builder
		fmt.Fprintf(&g, "package %s\n\n", pkg)
		if imports != "" {
			fmt.Fprintf(&g, "import (\n%s)\n\n", imports)
		}
		if len(pgVars) > 0 {
			fmt.Fprintf(&g, "var (\n%s\n)\n\nfunc init() {\n%s\n}\n\n", strings.Join(pgVars, "\n"), strings.Join(pgRegs, "\n"))
		}
		g.WriteString(body)
		return g.String(), h.String(), c.String()
	}
	imports := ""
	if strings.Contains(body, "unsafe.") {
		imports += "\t\"unsafe\"\n"
	}
	if strings.Contains(body, "runtime.") {
		imports += "\t\"runtime\"\n"
	}
	if rv.BaseImport != "" && strings.Contains(body, "qt.") {
		imports += "\tqt \"" + rv.BaseImport + "\"\n"
	}
	var g strings.Builder
	fmt.Fprintf(&g, "package %s\n\n/*\n#include <stdlib.h>\n#include \"gen_functions.h\"\n#include \"cppgo_shims.h\"\n*/\nimport \"C\"\n", pkg)
	if imports != "" {
		fmt.Fprintf(&g, "import (\n%s)\n", imports)
	}
	g.WriteString("\n")
	g.WriteString(body)
	return g.String(), h.String(), c.String()
}

// emitFuncCpp writes the shared C-ABI header decl + C++ shim body for one free function (identical
// for both backends — only the Go side differs).
func emitFuncCpp(hb, cb *strings.Builder, mm method, call string) {
	fmt.Fprintf(hb, "%s %s(%s);\n", retDeclCpp(mm), mm.cFunc, cSig(false, mm))
	var pre, post, cargs []string
	for _, p := range mm.params {
		pre = append(pre, p.cppPre...)
		post = append(post, p.cppPost...)
		cargs = append(cargs, p.cppArg)
	}
	fmt.Fprintf(cb, "%s %s(%s) {\n", retDeclCpp(mm), mm.cFunc, cSig(false, mm))
	for _, s := range pre {
		fmt.Fprintf(cb, "\t%s\n", s)
	}
	callExpr := call + "(" + strings.Join(cargs, ", ") + ")"
	if mm.ret.isVoid {
		fmt.Fprintf(cb, "\t%s;\n", callExpr)
		for _, s := range post {
			fmt.Fprintf(cb, "\t%s\n", s)
		}
	} else {
		fmt.Fprintf(cb, "\tauto&& __r = %s;\n", callExpr)
		for _, s := range post {
			fmt.Fprintf(cb, "\t%s\n", s)
		}
		for _, s := range mm.ret.cppPre {
			fmt.Fprintf(cb, "\t%s\n", s)
		}
		fmt.Fprintf(cb, "\treturn %s;\n", mm.ret.cppReturn)
	}
	fmt.Fprint(cb, "}\n")
}

// hasQObjectParentCtor reports whether ctors contains one callable as Cls((QObject*)parent): exactly
// one parameter, a pointer to the record named QObject. Such a ctor lets the generated subclass
// forward a parent; classes whose only ctors take other params (QMimeData, QCoreApplication) don't.
// hasQObjectParentCtor reports whether ctors has a single-(QObject* parent) ctor we can forward a parent
// through, and whether that parent is DEFAULTED (so a no-arg New<Cls>() form is also valid, like miqt).
