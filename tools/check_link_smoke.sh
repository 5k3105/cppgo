#!/usr/bin/env bash
# Link smoke-test matrix — the standing guard for the EXECUTABLE-LINK-ONLY bug class.
#
# A `go build ./pkg` builds a package ARCHIVE, which tolerates undefined externals and never resolves
# cross-package symbols. Those bugs (missing -l libs, unexported "in the header but not the lib" symbols,
# cross-package symbol collisions) appear ONLY when an EXECUTABLE links. This builds a TRIVIAL executable
# that imports each generated package — and the all-packages combo — forcing that link with no real app,
# no GUI, no app config. Run it every regen, right after the packages build.
#
# Each non-base package transitively imports the base `qt`, so its solo smoke-test already exercises the
# (base + that package) link — catching both its own unlinkable/missing-lib symbols AND any collision with
# the base. The ALL combo catches collisions between two non-base packages.
#
# Usage: check_link_smoke.sh [MODULE_DIR]   (default module: /tmp/cppgoqt)
set -uo pipefail
MOD="${1:-/tmp/cppgoqt}"
export GOTMPDIR="${GOTMPDIR:-$HOME/.cache/gobuild-tmp}" TMPDIR="${GOTMPDIR:-$HOME/.cache/gobuild-tmp}"
command -v ccache >/dev/null && export CC="ccache gcc" CXX="ccache g++"

# Generated packages = dirs with gen_support.go (skip hand-written ones like mainthread).
PKGS=$(for d in "$MOD"/*/; do [ -f "$d/gen_support.go" ] && basename "$d"; done)
[ -n "$PKGS" ] || { echo "no generated packages under $MOD"; exit 2; }

work=$(mktemp -d); fail=0
smoke() { # $1 = label, $2... = package names to import
  local label=$1; shift
  local d="$work/$label"; mkdir -p "$d"
  printf 'module smoke\n\ngo 1.26\n\nrequire cppgoqt v0.0.0\n\nreplace cppgoqt => %s\n' "$MOD" > "$d/go.mod"
  { echo 'package main'; echo 'import ('; for p in "$@"; do echo "	_ \"cppgoqt/$p\""; done; echo ')'; echo 'func main() {}'; } > "$d/main.go"
  if ( cd "$d" && go build -gcflags="-N -l" -o /dev/null . ) 2>"$d/err.txt"; then
    echo "  OK    $label"
  else
    fail=1; echo "  FAIL  $label:"
    grep -oE "undefined reference to \`[^']+'|multiple definition of \`[^']+'" "$d/err.txt" \
      | sort -u | sed 's/^/          /' | head -20
    grep -E "cannot find package|no required module" "$d/err.txt" | sed 's/^/          /' | head -3
  fi
}

echo ">> link smoke-test matrix (module: $MOD)"
for p in $PKGS; do smoke "$p" "$p"; done
# All generated packages together (catches non-base x non-base collisions, e.g. kf6 x webengine).
smoke "ALL" $PKGS

rm -rf "$work"
[ $fail -eq 0 ] && echo ">> link smoke: ALL CLEAN" || echo ">> link smoke: FAILURES above"
exit $fail
