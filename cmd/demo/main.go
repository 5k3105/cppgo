// demo loads frontend IR and prints the bindability report — the proof that the
// architecture handles, by construction, the entire bug class that miqt patches by
// hand (scope-minimal names, typedef chains, blocked-base members, unrenderable
// templates, fixed-size arrays).
//
// Usage: go run ./cmd/demo out/*.json
package main

import (
	"fmt"
	"os"
	"sort"

	"github.com/5k3105/cppgo/closure"
	"github.com/5k3105/cppgo/ir"
	"github.com/5k3105/cppgo/render"
)

func sig(m ir.Method, bound map[string]bool) string {
	parts := make([]string, 0, len(m.Params))
	for _, p := range m.Params {
		r := render.RenderGo(p.Type, bound)
		g := r.Go
		if !r.OK {
			g = "<" + r.Reason + ">"
		}
		parts = append(parts, fmt.Sprintf("%s %s", p.Name, g))
	}
	ret := ""
	if m.Ret.Kind != "" {
		r := render.RenderGo(m.Ret, bound)
		if r.OK {
			ret = " -> " + r.Go
		} else {
			ret = " -> <" + r.Reason + ">"
		}
	}
	name := m.Name
	if name == "" {
		name = "(ctor)"
	}
	return fmt.Sprintf("%s(%s)%s", name, join(parts), ret)
}

func join(s []string) string {
	out := ""
	for i, x := range s {
		if i > 0 {
			out += ", "
		}
		out += x
	}
	return out
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: demo <ir.json>...")
		os.Exit(2)
	}
	m, err := ir.Load(os.Args[1:]...)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load:", err)
		os.Exit(1)
	}
	bound := closure.BoundSet(m)

	kept, dropped := 0, 0
	reasons := map[string]int{}

	for _, r := range m.Records {
		if !bound[r.USR] {
			fmt.Printf("\n=== %s  [EXCLUDED by policy]\n", r.Name)
			continue
		}
		fmt.Printf("\n=== %s", r.Name)
		for _, b := range r.Bases {
			fmt.Printf("  : %s(%s)", b.Name, b.Access)
		}
		fmt.Println()
		all := append(append([]ir.Method{}, r.Ctors...), r.Methods...)
		for _, mm := range all {
			okk, why := render.MethodRenderable(mm, bound)
			mark := "KEEP "
			if !okk {
				mark = "DROP "
				dropped++
				reasons[firstClause(why)]++
			} else {
				kept++
			}
			line := fmt.Sprintf("  %s %s", mark, sig(mm, bound))
			if !okk {
				line += "    // " + why
			}
			fmt.Println(line)
		}
	}

	fmt.Printf("\n---------------------------------------------\n")
	fmt.Printf("methods+ctors kept: %d   dropped: %d\n", kept, dropped)
	if len(reasons) > 0 {
		fmt.Println("drop reasons (auto-derived, no per-type rules):")
		keys := make([]string, 0, len(reasons))
		for k := range reasons {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return reasons[keys[i]] > reasons[keys[j]] })
		for _, k := range keys {
			fmt.Printf("  %3d  %s\n", reasons[k], k)
		}
	}
}

// firstClause collapses "param x: ..." / "return: ..." to the underlying reason,
// so the tally groups by root cause.
func firstClause(why string) string {
	for i := 0; i+1 < len(why); i++ {
		if why[i] == ':' && why[i+1] == ' ' {
			return why[i+2:]
		}
	}
	return why
}
