#!/usr/bin/env python3
"""Catch cross-package C-symbol collisions STATICALLY (no build) — the cheapest guard for one class of
executable-link-only bug.

Two generated packages linked into ONE executable must not export the same C symbol, or the link fails with
'multiple definition'. Per-class shims (`<Cls>_<method>`, `cppgoVirt<Cls>_...`) are unique via the class
name; the risk is the package-global free-function symbols (`cppgofn_*`) and any gen_support helper. This
scans every package's C-ABI headers (gen_*.h) for declared symbols and reports any symbol declared in 2+
packages — a guaranteed multiple-definition before any compile.

Usage: check_symbol_collisions.py [MODULE_DIR]   (default: /tmp/cppgoqt)
"""
import re, glob, os, sys
from collections import defaultdict

MOD = sys.argv[1] if len(sys.argv) > 1 else "/tmp/cppgoqt"

# A C function declaration in a gen_*.h: "<ret-type...> <symbol>(<params>);" at column 0.
DECL = re.compile(r'^[A-Za-z_][\w :*&<>,]*?\b([A-Za-z_]\w*)\s*\(', re.M)


def symbols(pkg_dir):
    syms = set()
    for h in glob.glob(os.path.join(pkg_dir, "*.h")):
        if h.endswith("_cgo_export.h"):
            continue
        txt = open(h, encoding="utf-8", errors="ignore").read()
        for m in DECL.finditer(txt):
            name = m.group(1)
            # keep only the exported C-ABI shim symbols; skip C++ keywords picked up by the loose regex
            if name in ("if", "for", "while", "return", "switch", "sizeof", "class", "struct"):
                continue
            syms.add(name)
    return syms


def main():
    pkgs = [d for d in sorted(glob.glob(os.path.join(MOD, "*/")))
            if glob.glob(os.path.join(d, "gen_*.h"))]
    if not pkgs:
        print(f"no generated packages with gen_*.h under {MOD}")
        return 2
    owner = defaultdict(list)
    for d in pkgs:
        name = os.path.basename(d.rstrip("/"))
        for s in symbols(d):
            owner[s].append(name)
    collisions = {s: ps for s, ps in owner.items() if len(ps) > 1}
    print(f"packages: {', '.join(os.path.basename(d.rstrip('/')) for d in pkgs)}")
    print(f"distinct C-ABI symbols: {len(owner)}")
    print(f"CROSS-PACKAGE COLLISIONS: {len(collisions)}")
    for s, ps in sorted(collisions.items()):
        print(f"  {s}  ->  {', '.join(sorted(ps))}")
    return 1 if collisions else 0


if __name__ == "__main__":
    sys.exit(main())
