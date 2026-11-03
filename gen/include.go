package gen

import (
	"os"
	"path/filepath"
	"strings"
)

// The frontend records each declaration's defining header as the absolute path it was parsed from
// (ir.Record.Header). Spelled that way in the shim sources, the generated tree only compiles where
// the library's headers sit at that exact path. CPPGO_INCLUDE_ROOTS (a path list, as -I would take
// it) names the include directories the shims will be compiled with: a header under one of them is
// spelled relative to it, as a system include (<QtCore/qobject.h>), so the generated tree builds on
// any machine whose compile passes the same -I. Unset, headers are spelled as recorded.

// includeRoots is CPPGO_INCLUDE_ROOTS, cleaned, longest first (a nested root wins over its parent).
func includeRoots() []string {
	var roots []string
	for _, r := range filepath.SplitList(os.Getenv("CPPGO_INCLUDE_ROOTS")) {
		if r = strings.TrimSpace(r); r != "" {
			roots = append(roots, filepath.Clean(r))
		}
	}
	for i := 1; i < len(roots); i++ { // insertion sort by length, longest first
		for j := i; j > 0 && len(roots[j]) > len(roots[j-1]); j-- {
			roots[j], roots[j-1] = roots[j-1], roots[j]
		}
	}
	return roots
}

// includeSpelling is h as it goes after #include: <rel> when h lies under an include root, else "h".
func includeSpelling(h string) string {
	clean := filepath.Clean(h)
	for _, r := range includeRoots() {
		if rel, ok := strings.CutPrefix(clean, r+string(filepath.Separator)); ok && rel != "" {
			return "<" + filepath.ToSlash(rel) + ">"
		}
	}
	return "\"" + h + "\""
}
