#!/usr/bin/env python3
"""Verify cppgo's NewClass<N> constructor ordinals MATCH miqt's, per class.

Porting a miqt app once needed a painful per-class ctor-remap (NewQVariant11->23, ...) because cppgo skipped
copy ctors and numbered differently. After binding copy ctors + the canonical ctorSort, cppgo's ordinals
should line up with miqt's. This check diffs them by NORMALIZED SIGNATURE: for each class, does cppgo's
NewClass<N> take the same parameter types as miqt's NewClass<N>? Reports every class where they diverge.
Run after each regen; a shrinking divergence count means the remap step is going away.

Usage: check_ctor_parity.py [MIQT_QT_DIR] [CPPGO_QT_DIR]
"""
import re, glob, os, sys
from collections import defaultdict

MIQT = sys.argv[1] if len(sys.argv) > 1 else os.environ.get("MIQT_QT_DIR", "miqt/qt6")
if not os.path.isdir(MIQT):
    MIQT = MIQT.rstrip("6")
CPPGO = sys.argv[2] if len(sys.argv) > 2 else "/tmp/cppgoqt/qt"

CTOR = re.compile(r'^func (New([A-Za-z_]+?))(\d*)\((.*?)\) \*\2 \{', re.M)


def norm(params):
    # normalize a Go param list to a comparable type tuple (drop names, qt. prefix, leading *)
    if not params.strip():
        return ()
    out = []
    for p in params.split(','):
        p = p.strip()
        t = p.split(' ', 1)[1] if ' ' in p else p
        out.append(t.replace('qt.', '').lstrip('*').strip())
    return tuple(out)


def ctors(d):
    # class -> {ordinal(1 for bare New): sig}
    out = defaultdict(dict)
    for f in glob.glob(os.path.join(d, "*.go")):
        for m in CTOR.finditer(open(f, encoding="utf-8", errors="ignore").read()):
            cls, num, params = m.group(2), m.group(3), m.group(4)
            out[cls][int(num) if num else 1] = norm(params)
    return out


def main():
    if not os.path.isdir(MIQT) or not os.path.isdir(CPPGO):
        print("ERROR: a generated tree is missing (regen cppgo first).")
        return 2
    mi, cp = ctors(MIQT), ctors(CPPGO)
    shared = sorted(set(mi) & set(cp))
    aligned = diverged = 0
    examples = []
    for cls in shared:
        m, c = mi[cls], cp[cls]
        # compare the ordinal->signature maps over the common ordinals
        common = set(m) & set(c)
        mismatch = [n for n in common if m[n] != c[n]]
        if mismatch or set(m) != set(c):
            diverged += 1
            if len(examples) < 25:
                examples.append((cls, sorted(m.items()), sorted(c.items())))
        else:
            aligned += 1
    print(f"classes compared: {len(shared)}  aligned: {aligned}  DIVERGED: {diverged}")
    for cls, m, c in examples:
        print(f"\n  {cls}:")
        print(f"    miqt : {m}")
        print(f"    cppgo: {c}")
    return 1 if diverged else 0


if __name__ == "__main__":
    sys.exit(main())
