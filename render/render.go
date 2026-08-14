// Package render is the renderability decision for cppgo.
//
// The key property: RenderGo is a TOTAL FUNCTION over the structured IR. Given the
// set of bound record USRs, it deterministically returns either a Go type or a
// precise reason a type cannot be bound. There is no "does the string contain '<'"
// heuristic and no best-effort emit that can leak invalid Go — an unrenderable type
// is a decision, computed once. (Compare miqt, where QSpan<>/EulerAngles<>/T[N] each
// leaked into the output until patched by hand.)
package render

import (
	"fmt"
	"strings"

	"github.com/5k3105/cppgo/gen"
	"github.com/5k3105/cppgo/ir"
)

// renderableContainers are the template heads the emit can actually express.
// (In miqt these are scattered across QListOf/QSetOf/QMapOf/QPairOf/QFlagsOf; here
// they are one authoritative set, and anything else is an unrenderable template.)
var renderableContainers = map[string]bool{
	"QList": true, "QVector": true, "QSet": true,
	"QMap": true, "QHash": true, "QPair": true, "QFlags": true,
}

// GoName maps a qualified C++ name to a Go identifier (QFoo::Bar -> QFoo__Bar).
func GoName(qualified string) string {
	return strings.ReplaceAll(qualified, "::", "__")
}

// Result is the outcome of rendering a type.
type Result struct {
	Go     string // Go type expression (empty for void)
	OK     bool
	Reason string // why not, when !OK
}

func ok(s string) Result           { return Result{Go: s, OK: true} }
func no(f string, a ...any) Result { return Result{Reason: fmt.Sprintf(f, a...)} }

// RenderGo resolves a structured type to a Go type, or a reason it cannot be bound.
func RenderGo(t ir.Type, bound map[string]bool) Result {
	switch t.Kind {
	case "builtin":
		// One authority: the emitter's own builtin table (gen.BuiltinGo) — render/ previously kept a
		// private copy that drifted from what plan actually bound.
		if g, found := gen.BuiltinGo(t.Name); found {
			return ok(g)
		}
		return no("unmapped builtin %q", t.Name)

	case "enum":
		return ok(GoName(t.Name)) // enums always bind (int-backed)

	case "record":
		if bound[t.USR] {
			return ok(GoName(t.Name))
		}
		return no("unbound class %s", t.Name)

	case "pointer", "ref":
		if t.Elem == nil {
			return no("%s with no element", t.Kind)
		}
		inner := RenderGo(*t.Elem, bound)
		if !inner.OK {
			return inner
		}
		if inner.Go == "" { // void* / void&
			return ok("unsafe.Pointer")
		}
		return ok("*" + inner.Go)

	case "array":
		return no("fixed-size C array [%d] (not bindable as a field/param)", t.N)

	case "typedef":
		if t.Canonical == nil {
			return no("typedef %q with no canonical", t.Alias)
		}
		return RenderGo(*t.Canonical, bound) // resolve through clang's canonical — the WHOLE chain at once

	case "template":
		if !renderableContainers[t.Head] {
			return no("unrenderable template %s<...>", t.Head)
		}
		switch t.Head {
		case "QFlags":
			return ok("int")
		case "QList", "QVector", "QSet":
			if len(t.Args) != 1 {
				return no("%s arity", t.Head)
			}
			inner := RenderGo(t.Args[0], bound)
			if !inner.OK {
				return inner
			}
			return ok("[]" + inner.Go)
		case "QMap", "QHash":
			if len(t.Args) != 2 {
				return no("%s arity", t.Head)
			}
			k := RenderGo(t.Args[0], bound)
			v := RenderGo(t.Args[1], bound)
			if !k.OK {
				return k
			}
			if !v.OK {
				return v
			}
			return ok("map[" + k.Go + "]" + v.Go)
		case "QPair":
			return ok("QPair") // placeholder; structurally renderable
		}
		return no("unhandled container %s", t.Head)

	case "unexposed":
		return no("unexposed type %q", t.Spelling)

	default:
		return no("unknown type kind %q", t.Kind)
	}
}

// MethodRenderable reports whether every param + return type of m is renderable,
// returning the first blocking reason otherwise.
func MethodRenderable(m ir.Method, bound map[string]bool) (bool, string) {
	if m.Ret.Kind != "" { // ctors have no return
		if r := RenderGo(m.Ret, bound); !r.OK {
			return false, "return: " + r.Reason
		}
	}
	for _, p := range m.Params {
		if r := RenderGo(p.Type, bound); !r.OK {
			return false, fmt.Sprintf("param %s: %s", p.Name, r.Reason)
		}
	}
	return true, ""
}
