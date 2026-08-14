#!/usr/bin/env bash
# Derive the "in the header but not in the library" residual FROM THE LINK — the only stage that knows what
# the .so actually exports. (The consumed header can't tell an exported class from an unexported one on
# Linux; a frontend heuristic is unsound — see the playbook.) Links a trivial executable per built package
# (the smoke-test), parses every 'undefined reference to `Owner::symbol`' into its owner, and reports the
# CLASS-scope owners for cmd/gen/unlinkable.txt (and NAMESPACE owners, which the frontend handles by prefix,
# e.g. QtPrivate::). No regen: uses the packages you already built. Re-run after a regen; if the class list
# changed, update cmd/gen/unlinkable.txt and re-gen once (a fixpoint).
#
# Usage:
#   derive_unlinkable.sh [MODULE_DIR]              # link the built packages and derive
#   derive_unlinkable.sh --parse <link-error-log>  # parse owners from a saved link log (no build; for tests)
set -uo pipefail

# owners <file>: every undefined-reference OWNER (the C++ scope before the final ::), deduped.
owners() {
	grep -oE "undefined reference to \`[^']+'" "$1" 2>/dev/null \
		| sed "s/undefined reference to .//; s/'$//" \
		| sed -E 's/\(.*//'            `# drop the parameter list` \
		| sed -E 's/::[^:]+$//'        `# drop the final ::member -> owner (class or namespace)` \
		| sort -u
}

classify() {  # split owners into namespace-scope (contains ::) vs class-scope (no ::)
	local f=$1
	echo ">> NAMESPACE owners (handle by prefix in the frontend, e.g. QtPrivate::):"
	owners "$f" | grep '::' | sed 's/^/     /' || true
	echo ">> CLASS owners (-> cmd/gen/unlinkable.txt):"
	owners "$f" | grep -v '::'  | sed 's/^/     /' || true
	owners "$f" | grep -v '::'   # also emit clean to stdout tail for capture
}

if [ "${1:-}" = "--parse" ]; then
	[ -f "${2:-}" ] || { echo "usage: $0 --parse <link-error-log>"; exit 2; }
	classify "$2"
	exit 0
fi

MOD="${1:-/tmp/cppgoqt}"
export GOTMPDIR="${GOTMPDIR:-$HOME/.cache/gobuild-tmp}" TMPDIR="${GOTMPDIR:-$HOME/.cache/gobuild-tmp}"
command -v ccache >/dev/null && export CC="ccache gcc" CXX="ccache g++"
PKGS=$(for d in "$MOD"/*/; do [ -f "$d/gen_support.go" ] && basename "$d"; done)
[ -n "$PKGS" ] || { echo "no generated packages under $MOD"; exit 2; }

work=$(mktemp -d); allerr="$work/all.err"; : > "$allerr"
for p in $PKGS; do
	d="$work/$p"; mkdir -p "$d"
	printf 'module smoke\n\ngo 1.26\n\nrequire cppgoqt v0.0.0\n\nreplace cppgoqt => %s\n' "$MOD" > "$d/go.mod"
	printf 'package main\nimport _ "cppgoqt/%s"\nfunc main() {}\n' "$p" > "$d/main.go"
	( cd "$d" && go build -gcflags="-N -l" -o /dev/null . ) 2>>"$allerr" || true
done
echo ">> derived from linking: $PKGS"
classify "$allerr"
rm -rf "$work"
