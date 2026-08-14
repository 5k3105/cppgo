package gen

import (
	"strings"
	"testing"

	"github.com/5k3105/cppgo/closure"
	"github.com/5k3105/cppgo/ir"
)

// pruneTestPkgOf mirrors cmd/gen.pkgOf (unexported there) for the Resolver setup.
func pruneTestPkgOf(h string) string {
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

// TestFixpointPrunesDeadRecords checks Fixpoint against the cached qt base IR: it must drop the known
// opaque all-boilerplate handles (QWidgetData/QInternal) while keeping referenced zero-member records
// (QStringConverterBase, a base of QStringEncoder/Decoder). Skips when the cached IR is absent.
func TestFixpointPrunesDeadRecords(t *testing.T) {
	const irPath = "/tmp/qtbase.json"
	m, err := ir.Load(irPath)
	if err != nil {
		t.Skipf("cached IR %s not present: %v", irPath, err)
	}
	rv := Resolver{
		Bound: closure.BoundSet(m), Name: map[string]string{}, Copyable: map[string]bool{},
		Header: map[string]string{}, EnumName: map[string]string{}, EnumHeader: map[string]string{},
		Pkg: map[string]string{}, EnumPkg: map[string]string{}, Self: "qt",
	}
	for _, r := range m.Records {
		rv.Name[r.USR] = r.Name
		rv.Copyable[r.USR] = r.Copyable
		rv.Header[r.USR] = r.Header
		rv.Pkg[r.USR] = pruneTestPkgOf(r.Header)
	}
	for _, e := range m.Enums {
		rv.EnumName[e.USR] = e.Name
		rv.EnumHeader[e.USR] = e.Header
		rv.EnumPkg[e.USR] = pruneTestPkgOf(e.Header)
	}
	rv.MethodKeys = BuildMethodKeys(m.Records)
	rv.FreeOps = map[string][]ir.Func{}

	before := len(rv.Bound)
	pruned := Fixpoint(m, &rv)
	t.Logf("bound %d -> %d; pruned %d: %s", before, len(rv.Bound), len(pruned), strings.Join(pruned, ", "))

	prunedSet := map[string]bool{}
	for _, n := range pruned {
		prunedSet[n] = true
	}
	// (QWidgetData used to be on this list; since public FIELDS bind as accessors it renders
	// members and rightly survives.)
	for _, want := range []string{"QAbstractConcatenable", "QInternal"} {
		if !prunedSet[want] {
			t.Errorf("expected Fixpoint to prune dead record %q, but it did not", want)
		}
	}
	// A zero-member record that IS referenced (as a base) must survive.
	for _, keep := range []string{"QStringConverterBase"} {
		if prunedSet[keep] {
			t.Errorf("Fixpoint wrongly pruned referenced record %q", keep)
		}
	}
	if len(pruned) == 0 {
		t.Errorf("expected Fixpoint to prune some records, got none")
	}
}
