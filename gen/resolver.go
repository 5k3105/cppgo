package gen

import (
	"fmt"
	"strings"

	"github.com/5k3105/cppgo/ir"
)

type Resolver struct {
	Bound      map[string]bool
	Name       map[string]string // record USR -> qualified C++ name
	Copyable   map[string]bool   // record USR -> copy ctor usable (gates by-value bind)
	Header     map[string]string // record USR -> real defining header (for precise per-shim includes)
	EnumName   map[string]string // enum USR -> qualified C++ name (for typed Go enums)
	EnumHeader map[string]string // enum USR -> real defining header
	EnumPublic map[string]bool   // enum USR -> nameable by a free function (top-level/public-nested)
	Enum64     map[string]bool   // enum USR -> has enumerators outside 32 bits -> crosses the ABI as long long, not int
	Pkg        map[string]string // record USR -> owning Go package ("qt","kf6","webengine","pdf")
	EnumPkg    map[string]string // enum USR -> owning Go package
	// Records indexes the loaded module's records by USR (set by cmd/gen). Used to walk a class's
	// primary-base CHAIN so bind<Cls> can fetch every base-subobject pointer in ONE shim call
	// (<cn>_chain) instead of one crossing per inheritance level. nil (e.g. in emitter-only tests)
	// disables the chain optimization; bind falls back to the per-level <cn>_base calls.
	Records    map[string]*ir.Record
	Self       string // the package currently being emitted (others are cross-package)
	BaseImport string // import path of the base "qt" package (empty when emitting the base itself)
	Backend    string // "cgo" (default) or "purego" — selects the Go-wrapper emit path
	// MethodKeys maps a record USR -> the set of signature keys of its FLATTENED methods. Used to
	// de-flatten: a derived class skips re-emitting any non-static, non-protected method already present
	// in its embedded primary base (it inherits that method via Go struct embedding instead). This is
	// what brings cppgo to miqt's method count (no inherited-method re-emission) so the package compiles.
	MethodKeys map[string]map[string]bool
	// FreeOps maps a record USR -> free/friend operators whose FIRST argument is that record, bound as
	// methods on it (operator syntax in C++). Free non-operator functions are emitted package-level.
	FreeOps map[string][]ir.Func
	// Ancestors maps a record USR -> the set of ALL its transitive base USRs. Used to detect the covariant
	// trap: a virtual returning a record BY VALUE whose type derives from the declaring class's own base
	// hierarchy (KConfigBase::groupImpl -> KConfigGroup : KConfigBase) can't be overridden (GCC "invalid
	// covariant return type"); a record return unrelated to the class (moveCursor -> QModelIndex) is fine.
	Ancestors map[string]map[string]bool
}

// baseEmit describes a record's primary (embedded) base for the miqt-style embedding model: the Go
// struct embeds *<goType> and inherits its methods via promotion. cFunc is the C++ helper that performs
// the (multiple-inheritance-correct) static_cast from the derived pointer to the base subobject pointer.
type baseEmit struct {
	has     bool
	field   string // embed field name (the bare base type name, e.g. "QWidget")
	goType  string // embed field type, package-qualified as seen from Self (e.g. "qt.QWidget")
	bare    string // bare base Go name (for bind<bare>/UnsafeNew<bare>)
	pfx     string // "" (same package) or "qt." (cross-package)
	cross   bool   // base lives in another Go package
	cppName string // base C++ qualified name (for static_cast)
	cFunc   string // virtbase C helper name, "<cn>_base"
	baseUSR string
}

// primaryBase resolves r's first PUBLIC, bound base for struct embedding. ok=false => r is a root
// (no embeddable base): it carries the handle + override id directly.
func (rv Resolver) primaryBase(r ir.Record) (baseEmit, bool) {
	for _, b := range r.Bases {
		if b.Access != "public" || !rv.Bound[b.USR] {
			continue
		}
		goType, bare, cppName, pfx, ok := rv.rec(b.USR)
		if !ok {
			continue
		}
		cn := strings.ReplaceAll(r.Name, "::", "__")
		return baseEmit{has: true, field: bare, goType: goType, bare: bare, pfx: pfx,
			cross: pfx != "", cppName: cppName, cFunc: cn + "_base", baseUSR: b.USR}, true
	}
	return baseEmit{}, false
}

// baseChain walks r's primary-base chain (derived -> ... -> root), one baseEmit per level, as seen
// from the current package. complete=true means the walk terminated properly: it reached a true root
// (a record with no embeddable base) or a cross-package link (whose own package continues the chain).
// Only a COMPLETE chain of length >= 2 may drive the one-crossing <cn>_chain bind; anything else
// falls back to the per-level <cn>_base calls. Records==nil (emitter-only tests) yields incomplete.
func (rv Resolver) baseChain(r ir.Record) (chain []baseEmit, complete bool) {
	cur := r
	for {
		be, ok := rv.primaryBase(cur)
		if !ok {
			return chain, true // reached the root
		}
		chain = append(chain, be)
		if be.cross {
			return chain, true // the base package's own bind continues from here
		}
		if rv.Records == nil {
			return chain, false
		}
		next := rv.Records[be.baseUSR]
		if next == nil {
			return chain, false
		}
		cur = *next
	}
}

// secondaryBases returns every PUBLIC, bound base AFTER the primary (embedded) one — the
// multiple-inheritance bases Go can't embed (QWidget's QPaintDevice, QOpenGLWidget's
// QOpenGLFunctions, ...). Each gets an As<Base>() pointer-cast accessor (gap 1).
func (rv Resolver) secondaryBases(r ir.Record) []baseEmit {
	var out []baseEmit
	first := true
	cn := strings.ReplaceAll(r.Name, "::", "__")
	for _, b := range r.Bases {
		if b.Access != "public" || !rv.Bound[b.USR] {
			continue
		}
		goType, bare, cppName, pfx, ok := rv.rec(b.USR)
		if !ok {
			continue
		}
		if first { // the first bound public base is the embedded primary — skip it
			first = false
			continue
		}
		out = append(out, baseEmit{has: true, field: bare, goType: goType, bare: bare, pfx: pfx,
			cross: pfx != "", cppName: cppName, cFunc: cn + "_as" + bare, baseUSR: b.USR})
	}
	return out
}

// typeKey is a stable identity string for a type, used to match a derived class's inherited method
// against its base's copy (the frontend flattens by copying identical ir.Type nodes, so keys match).
func typeKey(t ir.Type) string {
	switch t.Kind {
	case "record", "enum":
		return t.Kind + ":" + t.USR
	case "builtin":
		return "b:" + t.Name
	case "pointer", "ref":
		e := ""
		if t.Elem != nil {
			e = typeKey(*t.Elem)
		}
		c := ""
		if t.Const {
			c = "c"
		}
		return t.Kind + c + "(" + e + ")"
	case "array":
		e := ""
		if t.Elem != nil {
			e = typeKey(*t.Elem)
		}
		return fmt.Sprintf("arr%d(%s)", t.N, e)
	case "template":
		s := t.Head + "<"
		for _, a := range t.Args {
			s += typeKey(a) + ","
		}
		return s + ">"
	case "typedef":
		if t.Canonical != nil {
			return typeKey(*t.Canonical)
		}
		return "td:" + t.Alias
	default:
		return t.Kind + ":" + t.Name + ":" + t.Spelling + ":" + t.Head
	}
}

// methodKey identifies a method by name + parameter signature + constness, independent of declaring
// class — so an inherited method and the base's own copy hash equal.
func methodKey(m ir.Method) string {
	var sb strings.Builder
	sb.WriteString(m.Name)
	sb.WriteByte('(')
	for i, p := range m.Params {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(typeKey(p.Type))
	}
	sb.WriteByte(')')
	if m.Const {
		sb.WriteString("const")
	}
	return sb.String()
}

// BuildMethodKeys precomputes, per record USR, the signature-key set of its flattened methods (default
// args expanded, matching the per-class emission). The Resolver uses it to de-flatten inherited methods.
func BuildMethodKeys(recs []ir.Record) map[string]map[string]bool {
	out := make(map[string]map[string]bool, len(recs))
	for i := range recs {
		r := &recs[i]
		keys := map[string]bool{}
		for _, m := range expandOptional(r.Methods) {
			keys[methodKey(m)] = true
		}
		out[r.USR] = keys
	}
	return out
}

// prefix returns the Go package qualifier ("qt.") for a type in package pkg as seen from Self, or ""
// when pkg is the current (self) package or unset. Cross-package references therefore become qt.QWidget
// and use the exported UnsafeNew*/UnsafePointer protocol; same-package references stay bare.
func (rv Resolver) prefix(pkg string) string {
	if pkg != "" && pkg != rv.Self {
		return pkg + "."
	}
	return ""
}

// rec resolves a record USR to its Go type (package-qualified), bare Go name (for UnsafeNew<bare>),
// C++ name, and package prefix. cppName never carries a Go package — the C++ shim sees raw Qt types.
func (rv Resolver) rec(usr string) (goType, bare, cppName, pfx string, ok bool) {
	if !rv.Bound[usr] {
		return "", "", "", "", false
	}
	n, has := rv.Name[usr]
	if !has {
		return "", "", "", "", false
	}
	bare = strings.ReplaceAll(n, "::", "__")
	pfx = rv.prefix(rv.Pkg[usr])
	return pfx + bare, bare, n, pfx, true
}
