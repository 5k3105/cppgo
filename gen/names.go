package gen

import (
	"strings"

	"github.com/5k3105/cppgo/ir"
)

// dataModel is the TARGET C data model: "lp64" (Linux/macOS x86-64 — `long` is 8 bytes) or
// "llp64" (Windows — `long` is 4 bytes). It only affects the handful of width-ambiguous C types
// below; everything else (int, long long, pointers, size_t-via-canonical) is identical. Set once
// by cmd/gen from CPPGO_DATAMODEL before any planning. NOTE: the IR itself must be extracted from
// the TARGET platform's headers (clang resolves size_t etc. per target); this switch covers the
// spelled-`long`/`wchar_t` residue the canonical types don't disambiguate.
var dataModel = "lp64"

// SetDataModel selects the target C data model ("lp64" | "llp64"). Panics on anything else —
// a typo'd model silently generating wrong widths is exactly the bug class cppgo exists to kill.
func SetDataModel(m string) {
	if m != "lp64" && m != "llp64" {
		panic("cppgo: unknown CPPGO_DATAMODEL " + m + " (want lp64 or llp64)")
	}
	dataModel = m
}

func builtinMap(name string) (cType, goType string, ok bool) {
	switch name {
	case "void":
		return "void", "", true
	case "bool":
		return "int", "bool", true
	case "char", "signed char":
		return "char", "int8", true
	case "unsigned char":
		return "unsigned char", "byte", true
	case "short":
		return "short", "int16", true
	case "unsigned short":
		return "unsigned short", "uint16", true
	case "int":
		return "int", "int", true // Go int, like miqt — C int values fit, and matches user code
	case "unsigned int", "uint":
		return "unsigned int", "uint", true
	case "long":
		if dataModel == "llp64" {
			return "long", "int32", true // Windows: long is 4 bytes
		}
		return "long", "int", true
	case "unsigned long":
		// size_t canonicalizes to `unsigned long` on LP64 — without this mapping every size_t-taking
		// API silently dropped. On LLP64 (Windows) `unsigned long` is 4 bytes.
		if dataModel == "llp64" {
			return "unsigned long", "uint32", true
		}
		return "unsigned long", "uint", true
	case "long long":
		return "long long", "int64", true
	case "unsigned long long":
		return "unsigned long long", "uint64", true
	case "float":
		return "float", "float32", true
	case "double":
		return "double", "float64", true
	case "char16_t":
		// UTF code-unit builtins (QChar/QString::utf16 surfaces). They cross the ABI as their
		// same-width C integer type — C++ converts implicitly in both directions.
		return "unsigned short", "uint16", true
	case "char32_t":
		return "unsigned int", "uint32", true
	case "wchar_t":
		if dataModel == "llp64" {
			return "unsigned short", "uint16", true // Windows wchar_t is a 2-byte UTF-16 unit
		}
		return "int", "int32", true // Linux wchar_t is a 4-byte int
	}
	return "", "", false
}

// BuiltinGo exposes the builtin C++->Go type mapping so render/ (the standalone renderability
// checker) answers from the SAME table the emitter plans with — one authority, no drift.
func BuiltinGo(name string) (goType string, ok bool) {
	_, g, ok := builtinMap(name)
	return g, ok
}

// isAppClass reports whether cn is a QApplication-family class. Their only ctor is (int& argc,
// char** argv), which the normal marshalling can't bind — so we emit a hand-crafted New<Cls>(args
// []string), replicating miqt: malloc long-lived argc/argv (never freed) + LockOSThread.
func isAppClass(cn string) bool {
	return cn == "QApplication" || cn == "QGuiApplication" || cn == "QCoreApplication"
}

// GoEnumType is the Go type name for a C++ enum, miqt-style: a Qt-namespace enum is flat
// (Qt::ItemDataRole -> ItemDataRole, Qt::AlignmentFlag -> AlignmentFlag), a class enum is
// class-qualified (QFont::StyleHint -> QFont__StyleHint).
func GoEnumType(qualified string) string {
	if strings.HasPrefix(qualified, "Qt::") {
		return strings.ReplaceAll(qualified[len("Qt::"):], "::", "__")
	}
	return strings.ReplaceAll(qualified, "::", "__")
}

// GoEnumConst is the Go constant name, miqt-style: Qt-namespace (and global) enums expose FLAT
// constants (Qt::ItemDataRole::DisplayRole -> DisplayRole); a class enum exposes Class__Const, the
// enclosing CLASS plus the constant with the enum-type segment dropped (QFont::StyleHint::Monospace
// -> QFont__Monospace).
func GoEnumConst(enumQualified, constName string, scoped bool) string {
	constName = exported(constName) // EXPORT it: Qt::GlobalColor uses lowercase enumerators (red, black,
	// darkGray, ...) which would otherwise emit as unexported, unusable Go constants. miqt capitalizes too.
	scope := ""
	if i := strings.LastIndex(enumQualified, "::"); i >= 0 {
		scope = enumQualified[:i]
	}
	if scope == "" || scope == "Qt" {
		return constName
	}
	// scoped (`enum class`): KEEP the enum-type segment (Class__Enum__Const) — matching miqt — so a
	// flattened scoped enumerator can't collide with a sibling type/enum of the same name in the class
	// (QPdfDocument::Status::Error -> QPdfDocument__Status__Error, not QPdfDocument__Error which is also
	// the separate `enum Error` TYPE). Unscoped enums keep the historical flat Class__Const.
	if scoped {
		return strings.ReplaceAll(enumQualified, "::", "__") + "__" + constName
	}
	return strings.ReplaceAll(scope, "::", "__") + "__" + constName
}

func cgoName(cType string) string {
	switch cType {
	case "char":
		return "C.char"
	case "unsigned char":
		return "C.uchar"
	case "short":
		return "C.short"
	case "unsigned short":
		return "C.ushort"
	case "int":
		return "C.int"
	case "unsigned int":
		return "C.uint"
	case "long":
		return "C.long"
	case "unsigned long":
		return "C.ulong"
	case "long long":
		return "C.longlong"
	case "unsigned long long":
		return "C.ulonglong"
	case "float":
		return "C.float"
	case "double":
		return "C.double"
	}
	return "C.int"
}

// goReserved are identifiers a C++ param can't safely reuse: Go keywords (a hard syntax error,
// e.g. QVariant(QVariant::Type type)) PLUS the predeclared builtins our generated method bodies
// reference in the same scope — chiefly `len` (QString::replace(qsizetype i, qsizetype len, ...)
// shadows the builtin, breaking len(after_b)). Such a param is renamed with a trailing underscore.
var goReserved = map[string]bool{
	"break": true, "case": true, "chan": true, "const": true, "continue": true, "default": true,
	"defer": true, "else": true, "fallthrough": true, "for": true, "func": true, "go": true,
	"goto": true, "if": true, "import": true, "interface": true, "map": true, "package": true,
	"range": true, "return": true, "select": true, "struct": true, "switch": true, "type": true,
	"var": true,
	// predeclared builtins our emitted bodies may call -> a same-named param would shadow them:
	"len": true, "cap": true, "make": true, "new": true, "append": true, "copy": true,
	"delete": true, "panic": true, "recover": true, "close": true, "print": true, "println": true,
	// predeclared TYPE/const names -> a param named `string` shadows the type in `string string`
	// (e.g. a Qt/KF6 setter whose argument is literally named `string`):
	"string": true, "bool": true, "byte": true, "rune": true, "error": true, "any": true,
	"int": true, "int8": true, "int16": true, "int32": true, "int64": true, "uint": true,
	"uint8": true, "uint16": true, "uint32": true, "uint64": true, "uintptr": true,
	"float32": true, "float64": true, "complex64": true, "complex128": true,
	"true": true, "false": true, "nil": true, "iota": true,
}

func goIdent(nm string) string {
	if goReserved[nm] {
		return nm + "_"
	}
	return nm
}

// typeLabel renders an ir.Type as a method-name suffix token (C++ type with spaces/:: removed), like
// miqt's renderTypeForMethod — used to disambiguate an unnamed 1-arg overload (e.g. NewQVariantWithInt).
func typeLabel(t ir.Type) string {
	s := t.Name
	switch t.Kind {
	case "pointer", "ref":
		if t.Elem != nil {
			s = t.Elem.Name
		}
	case "template":
		s = t.Head
	}
	return strings.NewReplacer(" ", "", "::", "", "*", "", "&", "").Replace(s)
}

// expandOptional replicates miqt's astTransformOptional: a non-virtual method/ctor with trailing
// default-argument params is expanded into one overload per arity (the shorter forms call the C++
// defaults). The original is truncated to its mandatory params; the longer forms are appended — the
// same order miqt uses, so the numbering matches. Virtuals are left intact (an override must match
// the full signature).
