#!/usr/bin/env python3
"""Verify cppgo emits a <X>Default for every virtual miqt gives a `super` parameter.

miqt's generated bindings are the oracle: a `func (this *C) On<X>(slot func(super func(...), ...))` means
class C's virtual <X> has a callable base impl, so cppgo must emit `func (this *C) <X>Default(...)`. This
script diffs the two generated trees and reports every (class, method) miqt has `super` for that cppgo is
missing — the exact Default-parity gap. Run it after each regen; exit code is nonzero if any gap remains.

Usage:
  check_default_parity.py [MIQT_QT_DIR] [CPPGO_QT_DIR]
Defaults:
  MIQT_QT_DIR  = os.environ.get("MIQT_QT_DIR", "miqt/qt6")  (falls back to .../qt)
  CPPGO_QT_DIR = /tmp/cppgoqt/qt
"""
import re, glob, os, sys
from collections import defaultdict

MIQT = sys.argv[1] if len(sys.argv) > 1 else os.environ.get("MIQT_QT_DIR", "miqt/qt6")
if not os.path.isdir(MIQT):
    MIQT = MIQT.rstrip("6")
CPPGO = sys.argv[2] if len(sys.argv) > 2 else "/tmp/cppgoqt/qt"

# miqt: a virtual with a base impl == an On<X> whose callback takes a `super func(...)` first param.
RE_SUPER = re.compile(r'func \(this \*(\w+)\) On(\w+)\(slot func\(super func')
# cppgo: the base-call helper.
RE_DEFAULT = re.compile(r'func \(this \*(\w+)\) (\w+)Default\(')


def scan(d, rx):
    pairs, names, byname = set(), set(), defaultdict(set)
    for f in glob.glob(os.path.join(d, "*.go")):
        for cls, x in rx.findall(open(f, encoding="utf-8", errors="ignore").read()):
            pairs.add((cls, x)); names.add(x); byname[cls].add(x)
    return pairs, names, byname


def main():
    if not os.path.isdir(MIQT) or not os.path.isdir(CPPGO):
        print(f"miqt={MIQT} exists={os.path.isdir(MIQT)}  cppgo={CPPGO} exists={os.path.isdir(CPPGO)}")
        print("ERROR: a generated tree is missing (regen cppgo first).")
        return 2

    m_pairs, m_names, _ = scan(MIQT, RE_SUPER)
    c_pairs, c_names, _ = scan(CPPGO, RE_DEFAULT)

    # Name-level diff is de-flatten-safe: cppgo emits a Default on the DECLARING class and inherits it via
    # Go embedding, while miqt re-emits per class. A method NAME in miqt-super but in NO cppgo Default
    # anywhere is a genuine gap.
    gap = sorted(m_names - c_names)
    print(f"miqt  super:   {len(m_pairs):5d} (class,method)  {len(m_names):4d} distinct names")
    print(f"cppgo Default: {len(c_pairs):5d} (class,method)  {len(c_names):4d} distinct names")
    print(f"\nDefault-parity GAP (miqt super, no cppgo Default for the name anywhere): {len(gap)}")
    if gap:
        overloaded = [g for g in gap if "With" in g or g[-1].isdigit()]
        base = [g for g in gap if g not in overloaded]
        print(f"  base-name gaps ({len(base)}): " + ", ".join(base))
        if overloaded:
            print(f"  overload-suffixed ({len(overloaded)}): " + ", ".join(overloaded))
    # cppgo SUPERSET signal: Defaults cppgo emits that miqt has no super for (informational, not a failure).
    extra = sorted(c_names - m_names)
    print(f"\ncppgo extra Defaults (superset beyond miqt's super set): {len(extra)}")
    if extra:
        print("  " + ", ".join(extra[:40]) + (" ..." if len(extra) > 40 else ""))

    return 1 if gap else 0


if __name__ == "__main__":
    sys.exit(main())
