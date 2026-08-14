// Package gen emits the three files that bind one C++ class: a Go cgo wrapper, a
// C-ABI header, and a C++ shim.
//
// Type support: scalars (void/integer/char/bool/enum/QFlags), bound records by handle,
// and QList<scalar|record>. Each parameter and the return are planned independently
// (paramPlan / retPlan), producing the C-ABI signature pieces plus the Go and C++
// statements needed to marshal across the boundary. Records cross as void*; lists cross
// as (array pointer, length). Ownership is explicit per wrapper:
//
//   - constructors and BY-VALUE returns  -> owned=true   (Go heap copy; Delete frees)
//   - pointer / reference returns        -> owned=false  (Qt owns; Delete is a no-op = UAF guard)
//   - list elements (returned)           -> owned=true   (each is a heap copy)
//
// Because the IR is structured and USR-keyed, classification is a clean walk over the
// type tree — there is no spelling to misread.
package gen

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/5k3105/cppgo/ir"
)

// Drop instrumentation. Every method/ctor/signal/virtual the generator declines to bind is COUNTED by
// reason — cmd/gen prints the summary to stderr on every run, so coverage gaps are always visible, never
// silent. Additionally, when CPPGO_DROPLOG names a file, each individual drop is logged with its type
// signature for authoritative auditing (instead of name-diffing against another binding).
var dropLogF *os.File

// probing is set by Fixpoint's renderability probe (prune.go): the probe runs the full EmitClass on
// records that may then be PRUNED, so its side effects — drop counts/log lines, shim-shape and
// callback-shape registration, verify-oracle lines — must be suppressed or the emitted support files
// reference classes that are never written.
var probing bool

type dropCount struct {
	reason string
	n      int
}

var dropCounts []dropCount

func init() {
	if p := os.Getenv("CPPGO_DROPLOG"); p != "" {
		f, err := os.Create(p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "cppgo: cannot create CPPGO_DROPLOG file %s: %v (drop log disabled)\n", p, err)
		} else {
			dropLogF = f
		}
	}
}

func dropLog(reason, cls, name string, m ir.Method) {
	if probing {
		return
	}
	hit := false
	for i := range dropCounts {
		if dropCounts[i].reason == reason {
			dropCounts[i].n++
			hit = true
			break
		}
	}
	if !hit {
		dropCounts = append(dropCounts, dropCount{reason, 1})
	}
	if dropLogF == nil {
		return
	}
	fmt.Fprintf(dropLogF, "%s\t%s::%s\t%s\n", reason, cls, name, methodTypes(m))
}

// DropSummary renders the per-reason drop counts ("method-planfail=412 signal-planfail=31 ...", sorted
// by count), or "" when nothing was dropped. cmd/gen prints it after every run.
func DropSummary() string {
	if len(dropCounts) == 0 {
		return ""
	}
	sorted := append([]dropCount{}, dropCounts...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].n > sorted[j].n })
	var parts []string
	total := 0
	for _, d := range sorted {
		parts = append(parts, fmt.Sprintf("%s=%d", d.reason, d.n))
		total += d.n
	}
	return fmt.Sprintf("%d declined bindings (set CPPGO_DROPLOG=<file> for the full list): %s",
		total, strings.Join(parts, " "))
}

// slotHash is the generator-time FNV-1a 64-bit hash of a virtual's slot key. It is a pure function of
// the key STRING, so the id for "paintEvent" is identical in every class and package — which is what
// lets a promoted base On<X> (registered through the embedded base wrapper) still match the derived
// __Go subclass's dispatch. Per-class collisions are checked at generation time (EmitClass panics).
func slotHash(s string) int64 {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	var h uint64 = offset64
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime64
	}
	return int64(h)
}

func methodTypes(m ir.Method) string {
	parts := []string{"ret=" + typeSummary(m.Ret)}
	for _, p := range m.Params {
		parts = append(parts, typeSummary(p.Type))
	}
	return strings.Join(parts, " ")
}

func typeSummary(t ir.Type) string {
	switch t.Kind {
	case "pointer", "ref":
		e := "?"
		if t.Elem != nil {
			e = typeSummary(*t.Elem)
		}
		c := ""
		if t.Const {
			c = "const "
		}
		return t.Kind + "->" + c + e
	case "record", "enum":
		return t.Kind + ":" + t.Name
	case "builtin":
		return t.Name
	case "template":
		return "tmpl:" + t.Head
	case "typedef":
		if t.Canonical != nil {
			return "td:" + t.Alias + "=" + typeSummary(*t.Canonical)
		}
		return "td:" + t.Alias
	default:
		return t.Kind + ":" + t.Spelling
	}
}

// Resolver answers: is this record bound, and what are its Go / C++ names?
func expandOptional(in []ir.Method) []ir.Method {
	out := append([]ir.Method{}, in...)
	var extra []ir.Method
	for j := range out {
		m := out[j]
		if m.Virtual {
			continue
		}
		opt := -1
		for k, p := range m.Params {
			if p.Optional {
				opt = k
				break
			}
		}
		if opt == -1 {
			continue
		}
		for x := opt; x < len(m.Params); x++ {
			v := m
			v.Params = append([]ir.Param{}, m.Params[:x+1]...)
			extra = append(extra, v)
		}
		out[j].Params = append([]ir.Param{}, m.Params[:opt]...)
	}
	return append(out, extra...)
}

func isQWidgetPtrCtor(m ir.Method) bool {
	return len(m.Params) == 1 && m.Params[0].Type.Kind == "pointer" &&
		m.Params[0].Type.Elem != nil && m.Params[0].Type.Elem.Name == "QWidget"
}

func isQVariantListCtor(m ir.Method) bool {
	return len(m.Params) == 1 && m.Params[0].Type.Kind == "template" &&
		m.Params[0].Type.Head == "QList" && len(m.Params[0].Type.Args) == 1 &&
		m.Params[0].Type.Args[0].Name == "QVariant"
}

// ctorSort replicates miqt's astTransformConstructorOrder: the (QWidget* parent) ctor sorts first
// (so New<Cls> is the parent ctor), a QVariantList ctor sorts last. Constructors are then numbered in
// this order (New<Cls>, New<Cls>2, ...).
func ctorSort(ctors []ir.Method) []ir.Method {
	out := append([]ir.Method{}, ctors...)
	sort.SliceStable(out, func(i, j int) bool {
		if isQWidgetPtrCtor(out[i]) && !isQWidgetPtrCtor(out[j]) {
			return true
		}
		if !isQVariantListCtor(out[i]) && isQVariantListCtor(out[j]) {
			return true
		}
		return false
	})
	return out
}

// operatorNames maps a C++ overloaded-operator member to its Go method name, matching miqt exactly so
// cppgo is a drop-in superset (the C++ call uses the named-operator form `((Cls*)self)->operator==(arg)`,
// which is valid for member operators). Operators NOT in this map are skipped: `operator=` (copy/move
// assign — low value in Go), `new`/`delete`, `&&`/`||`/`,` (lose their semantics across the boundary),
// and conversion operators (already dropped in the frontend).
var operatorNames = map[string]string{
	"operator=":  "OperatorAssign",
	"operator==": "OperatorEqual", "operator!=": "OperatorNotEqual",
	"operator<": "OperatorLesser", "operator<=": "OperatorLesserOrEqual",
	"operator>": "OperatorGreater", "operator>=": "OperatorGreaterOrEqual",
	"operator+": "OperatorPlus", "operator-": "OperatorMinus",
	"operator*": "OperatorMultiply", "operator/": "OperatorDivide", "operator%": "OperatorModulo",
	"operator+=": "OperatorPlusAssign", "operator-=": "OperatorMinusAssign",
	"operator*=": "OperatorMultiplyAssign", "operator/=": "OperatorDivideAssign",
	"operator%=": "OperatorModuloAssign",
	"operator&":  "OperatorBitwiseAnd", "operator|": "OperatorBitwiseOr", "operator^": "OperatorBitwiseXor",
	"operator~":  "OperatorBitwiseNot",
	"operator&=": "OperatorBitwiseAndAssign", "operator|=": "OperatorBitwiseOrAssign",
	"operator^=": "OperatorBitwiseXorAssign",
	"operator<<": "OperatorShiftLeft", "operator>>": "OperatorShiftRight",
	"operator<<=": "OperatorShiftLeftAssign", "operator>>=": "OperatorShiftRightAssign",
	"operator[]": "OperatorSubscript",
	"operator++": "OperatorPlusPlus", "operator--": "OperatorMinusMinus",
	"operator!": "OperatorNot", "operator()": "OperatorCall", "operator->": "OperatorMinusGreater",
}

// overloadName replicates miqt's astTransformOverloads: the first method with a name keeps it; a
// colliding 1-arg overload becomes <Base>With<ParamName> (or With<Type> when the param is unnamed),
// giving informative names like AddActionWithText/AddToolBarWithTitle; everything else falls back to a
// numeric <Base>2/3... `used` tracks taken names across the class.
func overloadName(base string, used map[string]int, m ir.Method) string {
	take := func(n string) string { used[n]++; return n }
	if used[base] == 0 {
		return take(base)
	}
	if len(m.Params) == 1 {
		p := m.Params[0]
		var cand string
		if len(p.Name) > 1 && !strings.HasPrefix(p.Name, "param") {
			cand = base + "With" + exported(p.Name)
		} else {
			cand = base + "With" + exported(typeLabel(p.Type))
		}
		if used[cand] == 0 {
			return take(cand)
		}
	}
	for ctr := 2; ; ctr++ {
		cand := fmt.Sprintf("%s%d", base, ctr)
		if used[cand] == 0 {
			return take(cand)
		}
	}
}

func exported(name string) string {
	if name == "" {
		return ""
	}
	return strings.ToUpper(name[:1]) + name[1:]
}

// EmitClass returns the Go wrapper, C-ABI header, and C++ shim for one class, plus hasExports = whether
// the shim references //export trampolines (signals/virtuals) — the unit writer uses it to include
// _cgo_export.h once per aggregate .cpp — and renderable = whether the class produced any real member
// (ctor/method/static/operator/Default, signal, or virtual). A non-renderable class is just an opaque
// handle with only boilerplate; Fixpoint (prune.go) drops the unreferenced ones.
// variantWrappable lists gui value types that register a metatype (Q_DECLARE_METATYPE), so
// QVariant::fromValue<T> compiles — these get a generated ToQVariant() (gap 2).
var variantWrappable = map[string]bool{
	"QBrush": true, "QColor": true, "QFont": true, "QIcon": true, "QPen": true,
	"QPixmap": true, "QImage": true, "QPalette": true, "QRegion": true, "QCursor": true,
	"QKeySequence": true, "QBitmap": true, "QPolygon": true, "QPolygonF": true,
	"QTransform": true, "QSizePolicy": true, "QTextLength": true, "QTextFormat": true,
}

func EmitClass(r ir.Record, pkg string, rv Resolver) (goSrc, hSrc, cppSrc string, hasExports, renderable bool) {
	cls := r.Name
	cn := strings.ReplaceAll(cls, "::", "__")

	// Embedding model (miqt-style): the Go struct embeds its primary public base and inherits that
	// base's methods via promotion, so they are NOT re-emitted here. baseKeys is the base's flattened
	// method-signature set used to skip those inherited methods below.
	base, _ := rv.primaryBase(r)
	baseKeys := rv.MethodKeys[base.baseUSR]

	cUsed, goUsed := map[string]int{}, map[string]int{}
	uniq := func(m map[string]int, base string) string {
		m[base]++
		if m[base] == 1 {
			return base
		}
		return fmt.Sprintf("%s%d", base, m[base])
	}

	var ms []method
	// Abstract classes (with pure virtuals) can't be `new`ed, so emit no constructors for them; they
	// remain bindable as return/param/base types.
	ctors := r.Ctors
	if r.Abstract {
		ctors = nil
	}
	// Expand default-args into overloads, then sort (QWidget*-ctor first). miqt names CONSTRUCTORS purely
	// numerically (New<Cls>, New<Cls>2, New<Cls>3 — confirmed: NewQTimer/NewQTimer2, NewQLabel..NewQLabel6),
	// and only METHODS get the With<Param> form — so ctors use numeric `uniq`, not `overloadName`.
	for _, m := range ctorSort(expandOptional(ctors)) {
		if mm, ok := plan(m, true, rv); ok {
			mm.cFunc = uniq(cUsed, cn+"_new")
			mm.goName = uniq(goUsed, "New"+cn)
			ms = append(ms, mm)
		} else {
			dropLog("ctor-planfail", cls, cls, m)
		}
	}
	expandedMethods := expandOptional(r.Methods)
	// Names this class EXTENDS with at least one overload the primary base lacks (a key not in baseKeys).
	// Go has no overloading, so a derived method name HIDES every promoted base method of that name — e.g.
	// QBoxLayout adds AddWidget(w,stretch), which would shadow the promoted QLayout.AddWidget(w), making
	// the 1-arg form uncallable. For such names we must NOT de-flatten the base-signature variants; re-emit
	// the whole name group (like miqt flattens). Purely-inherited names (objectName, ...) still de-flatten.
	extendedNames := map[string]bool{}
	if baseKeys != nil {
		for _, m := range expandedMethods {
			if !m.Static && !m.Protected && !baseKeys[methodKey(m)] {
				extendedNames[m.Name] = true
			}
		}
	}
	for _, m := range expandedMethods {
		// De-flatten: a non-static, non-protected method already provided by the embedded primary base is
		// inherited via Go promotion — don't re-emit it. (Statics have no receiver to promote through;
		// protected methods route through THIS class's __Go subclass, which only the most-derived class
		// has, so both must stay class-local.) Exception 1: a name this class extends (above) must be fully
		// re-emitted or its inherited overloads get shadowed in Go. Exception 2: a method THIS class
		// re-DECLARES (m.Own) that is NOT virtual is C++ name hiding — the base's shim would statically
		// call the BASE implementation, silently skipping the derived one — so re-emit it here (its shim
		// casts to the derived class, matching C++'s hiding semantics). Own virtuals still de-flatten:
		// the base shim's virtual call dynamically reaches the derived override. (Own is absent from IR
		// extracted by older frontends; it then defaults false = the previous behavior.)
		if baseKeys != nil && !m.Static && !m.Protected && baseKeys[methodKey(m)] && !extendedNames[m.Name] &&
			(!m.Own || m.Virtual) {
			continue
		}
		// Conversion operators (operator T()): bound as a To<T>() method (e.g. operator QString -> ToQString),
		// when the target T is bindable (cppRetSpelling non-empty — records/scalars/typedefs; pointer/ref
		// targets give "" and are dropped). convName != "" marks one; it bypasses the symbolic-operator gate.
		convName := ""
		if m.Conversion {
			if t := cppRetSpelling(m.Ret); t != "" {
				convName = "To" + convGoIdent(m.Ret)
			} else {
				dropLog("conversion-unbindable-target", cls, m.Name, m)
				continue
			}
		}
		// Operators: bind the valuable subset with miqt-style names (OperatorEqual, OperatorPlus, ...);
		// skip the rest (assignment, new/delete, &&/||/comma, ...). opName != "" marks an operator.
		opName := ""
		if !m.Conversion && strings.HasPrefix(m.Name, "operator") {
			opName = operatorNames[m.Name]
			if opName == "" {
				continue
			}
			// operator= is deleted on non-copyable classes (QObject-derived, QPdfDocument, ...); binding it
			// would call a deleted function. Bind it only where copy-assignment is actually usable.
			if opName == "OperatorAssign" && !rv.Copyable[r.USR] {
				continue
			}
		}
		// cppgo provides its own signal Disconnect(handle int64) helper; the raw QObject::disconnect
		// (now bindable since QMetaObject::Connection is exposed) would collide with it.
		if m.Name == "disconnect" {
			continue
		}
		// Protected methods: a non-virtual one is exposed through the __Go subclass (protCall — it adds a
		// `using Cls::member;` and the shim casts to the subclass). Protected statics/virtuals can't be
		// exposed this way (virtuals already get <X>Default), so skip them so gen never emits an
		// inaccessible call. (protCall methods are dropped below if the class isn't subclassable.)
		if m.Protected && (m.Virtual || m.Static) {
			dropLog("protected-virtual-or-static", cls, m.Name, m)
			continue
		}
		if m.Protected && sigHasInaccessibleEnum(m, rv) {
			// A protCall shim is a free function that names param/return types; a protected/private NESTED
			// enum (QGraphicsItem::Extension) can't be spelled there. Drop ONLY when such an inaccessible
			// enum appears — public + top-level enums (QAbstractItemView::CursorAction, ...) are fine.
			dropLog("protected-inaccessible-enum", cls, m.Name, m)
			continue
		}
		if mm, ok := plan(m, false, rv); ok {
			mm.protCall = m.Protected
			// Receiver methods are namespaced by their receiver type, so a bare exported name is fine.
			// Static methods become package-level funcs, so they must be class-prefixed to avoid
			// cross-class collisions (e.g. QByteArray::maxSize + QByteArrayView::maxSize). Operators use
			// their mapped name for BOTH the Go name and the C-ABI func (m.Name like "operator==" is not a
			// valid C identifier); the C++ call (mm.cppCall) keeps the operator spelling and works as
			// ((Cls*)self)->operator==(arg).
			switch {
			case convName != "":
				mm.cFunc = uniq(cUsed, cn+"_"+convName)
				mm.goName = overloadName(convName, goUsed, m)
			case opName != "":
				mm.cFunc = uniq(cUsed, cn+"_"+opName)
				mm.goName = overloadName(opName, goUsed, m)
			case mm.isStat:
				mm.cFunc = uniq(cUsed, cn+"_"+m.Name)
				mm.goName = overloadName(cn+"_"+exported(m.Name), goUsed, m)
			default:
				mm.cFunc = uniq(cUsed, cn+"_"+m.Name)
				mm.goName = overloadName(exported(m.Name), goUsed, m)
			}
			ms = append(ms, mm)
		} else {
			dropLog("method-planfail", cls, m.Name, m)
		}
	}

	// Free/friend operators whose FIRST argument is this record (operator==(QPoint,QPoint), ...), bound as
	// methods — superset of miqt, which only binds MEMBER operators. self is the first operand; the method
	// binds the rest; the C++ call uses operator syntax `(*(Cls*)self) OP (arg)`.
	for _, f := range rv.FreeOps[r.USR] {
		opName := operatorNames[f.Name]
		if opName == "" {
			continue
		}
		fm := ir.Method{Name: f.Name, Ret: f.Ret}
		if len(f.Params) > 1 {
			fm.Params = f.Params[1:] // drop the first operand (it is the receiver)
		}
		if mm, ok := plan(fm, false, rv); ok {
			mm.cFunc = uniq(cUsed, cn+"_"+opName)
			mm.goName = overloadName(opName, goUsed, fm)
			mm.freeOpSym = strings.TrimPrefix(f.Name, "operator")
			ms = append(ms, mm)
		}
	}

	// PUBLIC data members -> a Field() getter + SetField(v) setter, like miqt (cppgo otherwise binds
	// methods only, omitting QStyleOption flags/rect, QImage fields, QAccessible::State bitfields, ...).
	// Reuse plan()'s marshalling: the getter is a method (Ret = field type, no params); the setter is a void
	// method with one field-typed param. fieldGet/fieldSet retarget the C++ call to `recv->field` /
	// `recv->field = arg`. A field whose type can't be marshalled (plan fails) is silently skipped.
	voidT := ir.Type{Kind: "builtin", Name: "void"}
	for _, fld := range r.Fields {
		gm := ir.Method{Name: fld.Name, Ret: fld.Type}
		mm, ok := plan(gm, false, rv)
		if !ok {
			dropLog("field-get-planfail", cls, fld.Name, gm)
			continue
		}
		mm.fieldGet = fld.Name
		mm.cFunc = uniq(cUsed, cn+"_"+fld.Name+"_get")
		mm.goName = overloadName(exported(fld.Name), goUsed, gm)
		ms = append(ms, mm)
		if fld.Const {
			continue // const member: getter only
		}
		sm := ir.Method{Name: "set" + fld.Name, Ret: voidT, Params: []ir.Param{{Name: fld.Name, Type: fld.Type}}}
		if smm, ok := plan(sm, false, rv); ok {
			smm.fieldSet = fld.Name
			smm.cFunc = uniq(cUsed, cn+"_"+fld.Name+"_set")
			smm.goName = uniq(goUsed, "Set"+exported(fld.Name))
			ms = append(ms, smm)
		}
	}

	// Gap 1: secondary-base accessors. Go embeds only the PRIMARY base, so a multiple-inheritance base
	// (QWidget's QPaintDevice, QOpenGLWidget's QOpenGLFunctions, ...) is unreachable. Emit As<Base>() that
	// static_casts the derived pointer to the base subobject (the MI-correct adjustment), reusing the same
	// shim mechanism as the primary base.
	for _, sb := range rv.secondaryBases(r) {
		am := ir.Method{Name: "as" + sb.bare, Ret: ir.Type{Kind: "pointer", Elem: &ir.Type{Kind: "record", USR: sb.baseUSR, Name: sb.cppName}}}
		mm, ok := plan(am, false, rv)
		if !ok {
			continue
		}
		mm.baseCast = sb.cppName
		mm.cFunc = uniq(cUsed, cn+"_as"+sb.bare)
		mm.goName = uniq(goUsed, "As"+sb.bare)
		ms = append(ms, mm)
	}

	// Gap 2: ToQVariant() for gui value types with Q_DECLARE_METATYPE — boxes the value into a QVariant via
	// QVariant::fromValue<T> (templated; cppgo can't auto-bind it, so it's emitted as a hand-curated method,
	// like miqt's ToVariant helper). Allowlisted to types known to register a metatype (else fromValue won't
	// compile).
	if variantWrappable[cls] {
		var qvariantUSR string
		for usr, nm := range rv.Name {
			if nm == "QVariant" {
				qvariantUSR = usr
				break
			}
		}
		if qvariantUSR != "" {
			if rp, ok := planReturn(ir.Type{Kind: "record", USR: qvariantUSR, Name: "QVariant"}, rv); ok {
				ms = append(ms, method{goName: uniq(goUsed, "ToQVariant"), cFunc: uniq(cUsed, cn+"_toQVariant"), variantWrap: true, ret: rp})
			}
		}
	}

	// An overloaded signal name makes a plain &Class::member ambiguous; we disambiguate the member
	// pointer with a static_cast to the exact overload signature (emitCpp), and give each overload an
	// informative Go name via overloadName (On<Sig>, On<Sig>With<Param>...), exactly like methods.
	nameCount := map[string]int{}
	for _, m := range r.Methods {
		nameCount[m.Name]++
	}
	var sigs []signal
	for _, m := range r.Methods {
		if !m.Signal {
			continue
		}
		overloaded := nameCount[m.Name] > 1
		// A trailing QPrivateSignal tag is a private nested type we can't name in the static_cast, so an
		// overload that carries one can't be disambiguated — leave it dropped. (None of Qt's overloaded
		// signals carry it; this only guards the rare case.)
		if overloaded {
			if n := len(m.Params); n > 0 && strings.Contains(m.Params[n-1].Type.Name, "QPrivateSignal") {
				dropLog("signal-overloaded-private", cls, m.Name, m)
				continue
			}
		}
		if s, ok := planSignal(m, rv); ok {
			s.overloaded = overloaded
			// A signal with trailing default-valued args (clicked(bool checked=false), triggered(bool=false))
			// gets TWO bindings: OnClicked(func()) dropping the defaulted tail, and OnClickedWithChecked(
			// func(bool)) with it — the common no-arg case is clean, the rare checkable case still gets the
			// state. (Skipped for overloaded signals, which rely on the exact static_cast signature.)
			params := m.Params
			if n := len(params); n > 0 && strings.Contains(params[n-1].Type.Name, "QPrivateSignal") {
				params = params[:n-1]
			}
			n := len(s.args)
			req := n
			if !overloaded {
				for req > 0 && params[req-1].Optional {
					req--
				}
			}
			counts := []int{n}
			if req < n {
				counts = []int{req, n} // reduced first -> keeps the base name OnX; full -> OnXWith<Param>
			}
			for _, k := range counts {
				sv := s
				sv.exposed = k
				mk := m
				mk.Params = params[:k]
				sv.connectGo = overloadName("On"+exported(m.Name), goUsed, mk) // miqt-style: widget.OnClicked(fn)
				sv.connectC = uniq(cUsed, cn+"_connect_"+m.Name)
				sv.tramp = uniq(cUsed, "cppgoOn"+cn+"_"+m.Name)
				sigs = append(sigs, sv)
			}
		} else {
			dropLog("signal-planfail", cls, m.Name, m)
		}
	}

	// Virtual overrides. The generated C++ subclass must override EVERY pure virtual (incl. inherited)
	// to be concrete/constructible, so all of them must marshal; if any can't, the class can't be
	// subclassed from Go. Construction forwards a parent for QObject-derived classes, else the default
	// ctor. Declared (non-pure) virtuals are overridden best-effort.
	var virts []vmethod
	pureOK := true
	pureNames := map[string]bool{}
	for _, m := range r.PureVirtuals {
		v, ok := planVirt(m, rv)
		if ok && covariantRecordReturn(v.retRecordUSR, r.USR, rv) {
			ok = false // by-value record return deriving from a base -> can't override (covariant)
		}
		if !ok {
			dropLog("purevirtual-planfail", cls, m.Name, m)
			pureOK = false
			break
		}
		v.isPure = true
		pureNames[m.Name] = true
		virts = append(virts, v)
	}
	// A Go subclass <Cls>__Go must be derivable AND constructible. parentCtor = the base has a
	// single-(QObject*) ctor we can forward a parent through (true for QObject/QTimer/QAbstractListModel,
	// false for QMimeData/QCoreApplication, whose ctors take other params). Without one we fall back to
	// the default ctor; lacking both — or if the class is final / has a non-public dtor — we can't
	// subclass it, so emit no override machinery (its regular methods still bind).
	hasParent, parentOpt := hasQObjectParentCtor(r.Ctors)
	parentCtor := r.IsQObject && hasParent
	canSubclass := !r.Final && r.PublicDtor && (parentCtor || r.DefaultCtor)
	if canSubclass && pureOK {
		for _, m := range r.Methods {
			// pureNames are already overridden above. Overloaded virtuals ARE overridable (an override is a
			// re-declaration by signature, not an &Class::member that would be ambiguous) — they're
			// disambiguated by a unique slotKey below.
			if !m.Virtual || strings.HasPrefix(m.Name, "operator") || pureNames[m.Name] {
				continue
			}
			if v, ok := planVirt(m, rv); ok && !covariantRecordReturn(v.retRecordUSR, r.USR, rv) {
				virts = append(virts, v)
			} else {
				dropLog("virtual-planfail", cls, m.Name, m)
			}
		}
	} else {
		virts = nil // can't make a concrete subclass
	}
	slotKeys := map[string]int{}
	for i := range virts {
		virts[i].slotKey = uniq(slotKeys, virts[i].member) // unique per overload (member is shared)
		virts[i].idx = i                                   // position in the __Go subclass's override mask
		virts[i].slotID = slotHash(virts[i].slotKey)       // global registry key (same string -> same id everywhere)
		virts[i].onGo = uniq(goUsed, "On"+exported(virts[i].member))
		virts[i].tramp = uniq(cUsed, "cppgoVirt"+cn+"_"+virts[i].member)
	}
	// slotID collision within one class would misroute a registration — astronomically unlikely with
	// FNV-1a 64 over short member names, but check it at GENERATION time so it can never ship.
	for i := range virts {
		for j := i + 1; j < len(virts); j++ {
			if virts[i].slotID == virts[j].slotID {
				panic(fmt.Sprintf("cppgo: slot-id hash collision in %s: %q vs %q", cls, virts[i].slotKey, virts[j].slotKey))
			}
		}
	}

	// <Virtual>Default: invoke the BASE implementation (qualified, non-virtual call) — the clean
	// alternative to miqt's inlined `super` parameter (an override calls this.DataDefault(...) to defer
	// to the base). Clone each overridable virtual's already-bound method, retargeting the call to
	// Cls::member so it bypasses virtual dispatch.
	if len(virts) > 0 {
		// A virtual member is "callable as base" iff it has a NON-pure virtual here AND no pure overload of
		// the same name (a pure overload has no out-of-line symbol -> Cls::member would be an undefined ref).
		nonPure, pureV := map[string]bool{}, map[string]bool{}
		for _, v := range virts {
			if v.isPure {
				pureV[v.member] = true
			} else {
				nonPure[v.member] = true
			}
		}
		overridable := map[string]bool{}
		for mem := range nonPure {
			if !pureV[mem] {
				overridable[mem] = true
			}
		}
		handled := map[string]bool{} // member -> a Default was emitted (de-flat/protected passes then skip it)
		// byMember: <X>Default for every bound method that overrides a callable-as-base virtual. Done
		// per-METHOD (not per-member) so OVERLOADS each get their own Default (DrawLinesDefault,
		// DrawLines2Default) — C++ overload resolution picks the right base from each clone's own arg types.
		// (Enum nameability already handled by plan() when the method itself was bound into ms.)
		nms := len(ms)
		for i := 0; i < nms; i++ {
			m := ms[i]
			if m.isCtor || m.isStat || !overridable[m.cppCall] {
				continue
			}
			d := m // shallow copy; only the names + call target change
			d.goName = uniq(goUsed, m.goName+"Default")
			d.cFunc = uniq(cUsed, cn+"_"+m.cppCall+"_default")
			d.cppCall = cls + "::" + m.cppCall
			ms = append(ms, d)
			handled[m.cppCall] = true
		}
		for _, v := range virts {
			// PURE virtuals (no base symbol) and already-handled (byMember) members get no further Default.
			if v.isPure || handled[v.member] {
				continue
			}
			// A PUBLIC virtual this class CONCRETELY OVERRIDES but that was DE-FLATTENED (inherited via Go
			// embedding, so absent from byMember) still needs a <X>Default deferring to THIS class's impl —
			// e.g. QStyledItemDelegate::paint over the PURE QAbstractItemDelegate::paint. Plan it fresh and
			// target the qualified non-virtual `Cls::member` (public -> no forwarder). One per member.
			emitted := false
			for i := range r.Methods {
				om := r.Methods[i]
				if om.Name != v.member || !om.Virtual || om.Protected || om.Pure {
					continue
				}
				emitted = true // commit to this member (don't also fall through to the protected pass)
				// Skip the Default only if the signature names an INACCESSIBLE (protected/nested) enum the
				// free shim can't spell; public/top-level enums are fine (matches miqt's super coverage).
				if sigHasInaccessibleEnum(om, rv) {
					break
				}
				if mm, ok := plan(om, false, rv); ok {
					mm.goName = uniq(goUsed, exported(v.member)+"Default")
					mm.cFunc = uniq(cUsed, cn+"_"+v.member+"_default")
					mm.cppCall = cls + "::" + v.member
					ms = append(ms, mm)
				}
				break
			}
			if emitted {
				handled[v.member] = true
				continue
			}
			// A PROTECTED virtual (wheelEvent/keyPressEvent/dragMoveEvent/setData/...) has no public method
			// to clone, so it never reached byMember — but a Go override still needs to call the base. Plan
			// it as a method and emit <X>Default routed through the __Go subclass (qualified, non-virtual).
			// (We're inside len(virts)>0, so the class is subclassable and Cls__Go exists. Skip pure
			// virtuals: there's no base implementation to call.)
			if pureNames[v.member] {
				continue
			}
			for i := range r.Methods {
				om := r.Methods[i]
				if om.Name != v.member || !om.Virtual || !om.Protected {
					continue
				}
				if mm, ok := plan(om, false, rv); ok {
					mm.subclassRecv = true
					mm.goName = uniq(goUsed, exported(v.member)+"Default")
					mm.cFunc = uniq(cUsed, cn+"_"+v.member+"_default")
					// A free shim has NO protected access to Cls::member, so the qualified base call goes
					// through a PUBLIC forwarder declared on the __Go subclass (a member with the access). The
					// shim calls ((Cls__Go*)self)->cppgoBase_<member>(args). ENUM args cross the shim->forwarder
					// boundary as int — a free shim can't spell a protected/nested enum (QGraphicsItem::Extension)
					// — and the cast back happens INSIDE the forwarder, which has the access. (Mirrors miqt's
					// CABI: enum -> int. An enum RETURN is already fine: the shim deduces it via auto and casts
					// TO int, never spelling the type.) This matches miqt's full super coverage for such methods.
					fwd := "cppgoBase_" + v.member
					var pdecls, pnames []string
					for i, a := range v.args {
						an := fmt.Sprintf("__a%d", i)
						if i < len(om.Params) && om.Params[i].Type.Kind == "enum" {
							pdecls = append(pdecls, "int "+an)
							pnames = append(pnames, "("+om.Params[i].Type.Name+")"+an)
							// Make the shim pass the RAW int (mm's C param), not "(Enum)(int)" which names the enum.
							if i < len(mm.params) && len(mm.params[i].cParams) == 1 {
								f := strings.Fields(mm.params[i].cParams[0])
								mm.params[i].cppArg = f[len(f)-1]
							}
						} else {
							pdecls = append(pdecls, a.cppType+" "+an)
							pnames = append(pnames, an)
						}
					}
					retKw := "return "
					if v.isVoid {
						retKw = ""
					}
					mm.subclassFwd = fmt.Sprintf("\t%s %s(%s) { %s%s::%s(%s); }\n",
						v.cppRet, fwd, strings.Join(pdecls, ", "), retKw, cls, v.member, strings.Join(pnames, ", "))
					mm.cppCall = fwd
					ms = append(ms, mm)
					handled[v.member] = true
				}
				break
			}
		}
	}

	// Protected methods reach the base impl only through the generated subclass; drop them when the
	// class isn't subclassable (no __Go to cast to).
	if len(virts) == 0 {
		kept := ms[:0]
		for _, m := range ms {
			if !m.protCall {
				kept = append(kept, m)
			}
		}
		ms = kept
	}

	// QObject lives in the base "qt" package; a subclass constructor in another package takes a
	// *qt.QObject parent. (Empty prefix when emitting the base package itself.)
	qobjType := "*" + rv.prefix("qt") + "QObject"
	// Primary-base CHAIN for the one-crossing bind: only a complete chain 2+ levels deep is worth a
	// dedicated <cn>_chain shim (a 1-level chain is already a single crossing via <cn>_base).
	chain, chainOK := rv.baseChain(r)
	if !chainOK || len(chain) < 2 {
		chain = nil
	}
	// Go-wrapper backend: cgo (default) or purego (the cgo-free backend). The C-ABI header + C++ shim
	// (emitHeader/emitCpp) are identical for both — purego dlsyms the same extern "C" shims.
	goSrc = emitGo(pkg, cn, ms, sigs, virts, r.IsQObject, r.HasMetaObj, parentCtor, parentOpt, r.PublicDtor, qobjType, rv.BaseImport, base, chain)
	if rv.Backend == "purego" {
		goSrc = emitGoPurego(pkg, cn, rv.BaseImport, ms, sigs, virts, r.IsQObject, parentCtor, parentOpt, r.PublicDtor, qobjType, base, chain)
	}
	return goSrc,
		emitHeader(cn, ms, sigs, virts, parentCtor, base, r.IsQObject, r.HasMetaObj, rv.Backend, len(chain)),
		emitCpp(cls, cn, r.Header, ms, sigs, virts, parentCtor, referencedHeaders(r, rv), r.PublicDtor, base, r.IsQObject, r.HasMetaObj, rv.Backend, chain),
		len(sigs) > 0 || len(virts) > 0,
		len(ms) > 0 || len(sigs) > 0 || len(virts) > 0
}

// referencedHeaders returns the real headers of every bound record/enum r's shim actually touches
// (bases + each ctor/method/virtual param & return, recursively). The shim then #includes exactly
// these plus r's own header — not the module umbrella — so each translation unit parses a handful of
// small headers instead of all of Qt. This is the whole-module build-time fix.
func hasQObjectParentCtor(ctors []ir.Method) (has, optional bool) {
	for _, c := range ctors {
		if len(c.Params) != 1 {
			continue
		}
		p := c.Params[0].Type
		if p.Kind == "pointer" && p.Elem != nil && p.Elem.Kind == "record" && p.Elem.Name == "QObject" {
			return true, c.Params[0].Optional
		}
	}
	return false, false
}
