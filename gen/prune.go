package gen

// Fixpoint implements closure's deferred prune (see closure/closure.go's TODO note): a record that
// closure binds but that renders ZERO members (no ctor/method/static/operator/Default, no signal, no
// virtual — just an opaque all-boilerplate handle) AND that nothing bindable references is dead weight
// in the output. Dropping it can make ANOTHER zero-member record become unreferenced, so we iterate to
// a fixpoint. Renderability is decided by the emitter (EmitClass), which is why this lives in gen, not
// closure (closure may not import gen).

import (
	"sort"

	"github.com/5k3105/cppgo/ir"
)

// collectTypeRefs adds every record USR reachable from t (through pointer/ref/array elem, template
// args, and typedef canonical) to into.
func collectTypeRefs(t ir.Type, into map[string]bool) {
	if t.Kind == "record" && t.USR != "" {
		into[t.USR] = true
	}
	if t.Elem != nil {
		collectTypeRefs(*t.Elem, into)
	}
	for i := range t.Args {
		collectTypeRefs(t.Args[i], into)
	}
	if t.Canonical != nil {
		collectTypeRefs(*t.Canonical, into)
	}
}

// referencedRecords returns every record USR used as a base of a bound record, or appearing in any
// param/return type of any bound record's ctors/methods/pureVirtuals. Broad on purpose: a record
// referenced anywhere bindable must survive even if it has no members of its own.
func referencedRecords(m *ir.Module, bound map[string]bool) map[string]bool {
	ref := map[string]bool{}
	addMethods := func(ms []ir.Method) {
		for i := range ms {
			collectTypeRefs(ms[i].Ret, ref)
			for j := range ms[i].Params {
				collectTypeRefs(ms[i].Params[j].Type, ref)
			}
		}
	}
	for i := range m.Records {
		r := &m.Records[i]
		if !bound[r.USR] {
			continue
		}
		for _, b := range r.Bases {
			if b.USR != "" {
				ref[b.USR] = true
			}
		}
		addMethods(r.Ctors)
		addMethods(r.Methods)
		addMethods(r.PureVirtuals)
	}
	return ref
}

// Fixpoint mutates rv.Bound, dropping zero-renderable unreferenced records until stable. It returns the
// pruned record names (sorted, in prune order). pkg only affects emitted imports, not member gathering,
// so rv.Self is used for the renderability probe.
func Fixpoint(m *ir.Module, rv *Resolver) []string {
	// The probe runs the FULL EmitClass on candidate records; `probing` suppresses its side effects
	// (drop counts/log, shim/callback shape registration, verify-oracle lines) so support files never
	// reference a class that ends up pruned and unwritten.
	probing = true
	defer func() { probing = false }()
	var pruned []string
	for {
		ref := referencedRecords(m, rv.Bound)
		var drop []string
		for i := range m.Records {
			r := &m.Records[i]
			if !rv.Bound[r.USR] || ref[r.USR] {
				continue
			}
			if _, _, _, _, renderable := EmitClass(*r, rv.Self, *rv); !renderable {
				drop = append(drop, r.USR)
			}
		}
		if len(drop) == 0 {
			return pruned
		}
		for _, usr := range drop {
			pruned = append(pruned, rv.Name[usr])
			delete(rv.Bound, usr)
		}
		sort.Strings(pruned)
	}
}
