#!/usr/bin/env python3
"""Method-coverage parity vs miqt: report method names miqt binds that cppgo does NOT (and cppgo's extras).

cppgo aims to be a SUPERSET of miqt. This diffs distinct method NAMES (de-flatten-safe: a name cppgo emits
on the declaring class is reachable everywhere via embedding). Run after each regen to watch the miqt-only
gap shrink and confirm cppgo stays a superset.

Usage: check_method_parity.py [MIQT_QT_DIR] [CPPGO_QT_DIR]
"""
import re, glob, os, sys

MIQT = sys.argv[1] if len(sys.argv) > 1 else os.environ.get("MIQT_QT_DIR", "miqt/qt6")
if not os.path.isdir(MIQT):
    MIQT = MIQT.rstrip("6")
CPPGO = sys.argv[2] if len(sys.argv) > 2 else "/tmp/cppgoqt/qt"

RX = re.compile(r'func \(this \*\w+\) (\w+)\(')


def names(d):
    s = set()
    for f in glob.glob(os.path.join(d, "*.go")):
        for m in RX.findall(open(f, encoding="utf-8", errors="ignore").read()):
            if m.startswith("On") or m.endswith("Default") or m.startswith("Unsafe"):
                continue
            s.add(m)
    return s


def main():
    if not os.path.isdir(MIQT) or not os.path.isdir(CPPGO):
        print("ERROR: a generated tree is missing (regen cppgo first).")
        return 2
    m, c = names(MIQT), names(CPPGO)
    gap = sorted(m - c)
    extra = sorted(c - m)
    itr = [g for g in gap if "Iterator" in g or g.endswith("Iter")]
    print(f"miqt method names: {len(m)}   cppgo: {len(c)}   (cppgo {'IS' if len(c) >= len(m) else 'is NOT'} a superset by count)")
    print(f"miqt-only (candidate gaps): {len(gap)}  [iterator skips: {len(itr)}]")
    print(f"cppgo-only (superset beyond miqt): {len(extra)}")
    if gap:
        print("  gap sample:", ", ".join(g for g in gap if g not in itr)[:600])
    return 1 if (len(gap) - len(itr)) > 0 else 0


if __name__ == "__main__":
    sys.exit(main())
