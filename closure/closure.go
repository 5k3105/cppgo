// Package closure decides which records to bind.
//
// Policy seed: every record the frontend emitted, minus an explicit policy exclusion
// (private/internal names). This replaces miqt's reactive per-type blocklist for the
// common case. On top of this seed, gen.Fixpoint computes a true FIXPOINT — drop a
// record that renders no members AND that nothing bindable references, then re-run —
// so opaque all-boilerplate handles (QWidgetData/QInternal/...) and their now-dead
// dependents prune automatically. That pass lives in gen, not here, because
// renderability is decided by the emitter (EmitClass) and closure must not import gen.
// (Measured 2026-06-20: it drops 18 of the qt package's 685 bound records.)
package closure

import (
	"strings"

	"github.com/5k3105/cppgo/ir"
)

// excluded is the explicit POLICY layer (not a reactive leak-list): genuinely
// private/internal types we never want to project.
func excluded(name string) bool {
	return name == "" ||
		strings.Contains(name, "Private") ||
		strings.HasPrefix(name, "QtPrivate::") ||
		strings.HasPrefix(name, "QtSharedPointer::") || // internal refcount impl ns for QSharedPointer
		strings.Contains(name, "::Interface") || // QPA / internal interface structs
		strings.ContainsAny(name, "(<> ") || // anonymous/unnamed namespaces, leaked templates, etc.
		lowercaseNamespace(name) // q20::/qxp::/std:: etc. — Qt public API is never in a lowercase top-level ns
}

// lowercaseNamespace reports whether name's top-level namespace segment starts with a lowercase
// letter. Qt's public classes are global or live under uppercase scopes (Qt::, QFoo::...); the
// lowercase top-level namespaces (q20, q23, qxp, std, ...) are internal C++-backport / detail helpers.
func lowercaseNamespace(name string) bool {
	i := strings.Index(name, "::")
	if i <= 0 {
		return false
	}
	c := name[0]
	return c >= 'a' && c <= 'z'
}

// privateHeader reports whether a record's defining header is a Qt-private one: a `_p.h`/`_impl.h`
// file or anything under a private/ directory. Such headers are not public API (some, e.g.
// qrangemodel_impl.h, `#error` on direct inclusion), so a type defined only there is an internal
// detail we must not bind — and, with per-shim minimal includes, must not try to #include.
func privateHeader(h string) bool {
	base := h
	if i := strings.LastIndexByte(h, '/'); i >= 0 {
		base = h[i+1:]
	}
	return strings.HasSuffix(base, "_p.h") || strings.HasSuffix(base, "_impl.h") ||
		strings.Contains(h, "/private/")
}

// BoundSet returns the USR set of records chosen to bind.
func BoundSet(m *ir.Module) map[string]bool {
	bound := map[string]bool{}
	for _, r := range m.Records {
		if !excluded(r.Name) && !privateHeader(r.Header) {
			bound[r.USR] = true
		}
	}
	// The fixpoint prune (zero-renderable + unreferenced records) runs after this, in
	// gen.Fixpoint — it needs the emitter to decide renderability, which this package
	// can't reach without an import cycle. cmd/gen calls it once the Resolver is built.
	return bound
}
