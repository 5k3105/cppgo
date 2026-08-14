#!/usr/bin/env bash
# Restore the leg-1 forked cgo (cmd/cppgocgo) from its tracked copy in this repo into GOROOT.
#
# The fork MUST live at $GOROOT/src/cmd/cppgocgo to (a) access cmd/internal/* and (b) be accepted as the
# cgo tool by `go build -toolexec`. But GOROOT is outside version control and gets wiped by Go upgrades /
# rebuilt toolchains — so the authoritative source is cgofork/cppgocgo/ HERE, and this script syncs it in.
#
# Workflow: edit the splice in cgofork/cppgocgo/ (NOT in GOROOT — a sync overwrites GOROOT). all.sh runs
# this before building /tmp/cgofork/cgo, so the repo copy is always the source of truth.
set -e
REPO_FORK="$(cd "$(dirname "$0")" && pwd)/cppgocgo"
GOROOT_FORK="$(go env GOROOT)/src/cmd/cppgocgo"
[ -d "$REPO_FORK" ] || { echo "restore_cppgocgo: missing $REPO_FORK" >&2; exit 1; }
if [ ! -f "$GOROOT_FORK/proto.go" ] || [ "$REPO_FORK/proto.go" -nt "$GOROOT_FORK/proto.go" ]; then
	rm -rf "$GOROOT_FORK"
	cp -r "$REPO_FORK" "$GOROOT_FORK"
	# The repo copy carries its own go.mod (so the cppgo module's `go build ./...` skips it — the fork
	# only compiles inside GOROOT, where it may import cmd/internal/*); INSIDE GOROOT that go.mod would
	# hide the package from the std build, so drop it after the sync.
	rm -f "$GOROOT_FORK/go.mod"
	echo ">> restored cmd/cppgocgo from repo -> $GOROOT_FORK"
else
	echo ">> cmd/cppgocgo in GOROOT already current"
fi
