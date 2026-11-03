package gen

import "testing"

// A header under an include root is spelled relative to it (<...>), the longest root winning; any
// other header, and every header with no roots set, is spelled as recorded ("...").
func TestIncludeSpelling(t *testing.T) {
	t.Setenv("CPPGO_INCLUDE_ROOTS", "/opt/qt/include:/opt/qt/include/QtCore")
	for in, want := range map[string]string{
		"/opt/qt/include/QtWidgets/qwidget.h": "<QtWidgets/qwidget.h>",
		"/opt/qt/include/QtCore/qobject.h":    "<qobject.h>", // the nested root wins
		"/usr/include/stdio.h":                `"/usr/include/stdio.h"`,
		"/opt/qt/includex/a.h":                `"/opt/qt/includex/a.h"`, // a prefix that is not a directory
	} {
		if got := includeSpelling(in); got != want {
			t.Errorf("includeSpelling(%q) = %s, want %s", in, got, want)
		}
	}
	t.Setenv("CPPGO_INCLUDE_ROOTS", "")
	if got := includeSpelling("/opt/qt/include/QtCore/qobject.h"); got != `"/opt/qt/include/QtCore/qobject.h"` {
		t.Errorf("no roots: %s", got)
	}
}
