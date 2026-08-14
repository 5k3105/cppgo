#!/usr/bin/env bash
# Full regen of EVERY package in one shot: base qt (Core+Gui+Widgets) + kf6 + webengine + pdf, all in
# one cppgoqt module. Build-speed optimizations: ccache, precompiled
# header (PCH), -O0/-g0 + unity C++ TUs (from the generator), and frontend/JSON caching.
# Requires a Linux env with clang/LLVM, Qt 6 dev headers, and Go (see BUILDING.md).
set -uo pipefail
cd "$(dirname "$0")"

# Memory safety (measured: a full regen peaks ~12.8/14 GB — the serial Go compile is ~6.9 GB single-proc).
# Make THIS build tree the kernel's first OOM victim, so an overrun kills the build, not the editor/session.
echo 1000 > /proc/self/oom_score_adj 2>/dev/null || true
ROOT=/tmp/cppgoqt
KF6INC=$(for d in /usr/include/KF6/*/; do printf -- "-I%s " "$d"; done)
COMMON_CXX="$(pkg-config --cflags Qt6Widgets) $KF6INC"
QTLIBS="$(pkg-config --libs Qt6Widgets)"

# ccache: object cache by content hash, so an iterative regen recompiles only changed units. cgo honors
# CC/CXX. The default cache dir ($HOME/.ccache) and GOCACHE ($HOME/.cache/go-build) persist across builds
# ($HOME); in an ephemeral build container, volume-mount those dirs to keep the caches warm.
if command -v ccache >/dev/null; then
	export CC="ccache gcc" CXX="ccache g++"
	export CCACHE_MAXSIZE="${CCACHE_MAXSIZE:-25G}" # the Qt object cache is large; avoid premature eviction
	export CCACHE_DEPEND=1                          # depend mode: hash direct deps, not full preprocessed output
	# CRITICAL for PCH: without these sloppiness flags ccache REFUSES to cache any PCH-using compile, so the
	# PCH + ccache levers wouldn't compose at all.
	export CCACHE_SLOPPINESS="pch_defines,time_macros,include_file_mtime,include_file_ctime"
	echo ">> ccache enabled (depend mode + PCH sloppiness, maxsize $CCACHE_MAXSIZE)"
fi
CXXBIN="${CXX:-g++}"

# Build temp on the roomy /home partition, NOT the default /tmp (a ~7 GB tmpfs/RAM disk): the qt cgo build
# writes multi-GB of intermediate objects to $WORK, which overflows tmpfs ("no space left on device") and
# also burns RAM. /home has hundreds of GB free. Set both so go and any child compiler agree.
export GOTMPDIR="${GOTMPDIR:-$HOME/.cache/gobuild-tmp}"
export TMPDIR="$GOTMPDIR"
mkdir -p "$GOTMPDIR"

# C++ TU grouping (CPPGO_UNITS). Per-class (0) = fast edit→regen loop: a generator fix touching N classes
# recompiles only N small TUs (ccache/GOCACHE). Unity TUs (24) = ~6x faster COLD full regen by amortizing
# the 124 MB PCH + shared-header parse across a batch (measured: 9:58 vs ~60 min; 24 is the sweet spot —
# 48 risks the ~12.8/14 GB OOM). So: `./all.sh --full` (or FULL=1) for a cold regen; bare `./all.sh` for the
# iterative loop. An explicit CPPGO_UNITS env always wins.
case "${1:-}" in --full|full|-f) FULL=1 ;; esac
if [ -z "${CPPGO_UNITS:-}" ]; then export CPPGO_UNITS=$([ "${FULL:-0}" = 1 ] && echo 24 || echo 0); fi
echo ">> CPPGO_UNITS=$CPPGO_UNITS ($([ "$CPPGO_UNITS" = 0 ] && echo 'per-class — iterative' || echo 'unity TUs — full regen'))"

# (6) Frontend cache: build cppir2 only when its source is newer than the binary.
if [ ! -x ./cppir2 ] || [ frontend/cppir2.cpp -nt ./cppir2 ]; then
  echo ">> build cppir2"
  clang++ -std=c++17 -fno-rtti frontend/cppir2.cpp -o ./cppir2 -lclang-cpp -lLLVM 2>&1 | tail -2
fi

# (6) JSON cache: re-parse pdf IR only if missing or the frontend is newer than the cached JSON.
if [ ! -f /tmp/pdf.json ] || [ ./cppir2 -nt /tmp/pdf.json ]; then
  echo ">> extract pdf"
  echo "#include <QtPdfWidgets/QtPdfWidgets>" > /tmp/umbrella_pdf.h
  ./cppir2 /tmp/umbrella_pdf.h "QtCore,QtGui,QtWidgets,QtPdf" $(pkg-config --cflags Qt6PdfWidgets) > /tmp/pdf.json
fi

rm -rf "$ROOT"; mkdir -p "$ROOT"
printf 'module cppgoqt\n\ngo 1.26\n' > "$ROOT/go.mod"

# Prebuilt gen binary (avoids `go run` recompiling+wrapping the generator on each of the 4 invocations;
# go's build cache makes this rebuild a no-op when unchanged). NB: NOT named ./gen — that collides with
# the gen/ package directory.
echo ">> build gen"; go build -o ./cppgogen ./cmd/gen
echo ">> gen qt";        ./cppgogen "$ROOT/qt"        qt        -          all /tmp/qtbase.json
echo ">> gen kf6";       ./cppgogen "$ROOT/kf6"       kf6       cppgoqt/qt all /tmp/KF6.json
echo ">> gen webengine"; ./cppgogen "$ROOT/webengine" webengine cppgoqt/qt all /tmp/dep.json
echo ">> gen pdf";       ./cppgogen "$ROOT/pdf"       pdf       cppgoqt/qt all /tmp/pdf.json

# cgo flags: common compile flags everywhere; per-package link libs. (-O0 -g0 -include cppgo_pch.h are
# baked into the gen_support.go CXXFLAGS by the generator.)
KF6LIBS="-lKF6TextEditor -lKF6Parts -lKF6CoreAddons -lKF6Crash -lKF6WidgetsAddons -lKF6GuiAddons -lKF6ConfigCore -lKF6ConfigGui -lKF6ConfigWidgets -lKF6I18n -lKF6XmlGui -lKF6SyntaxHighlighting -lKF6Completion -lKF6KIOCore $QTLIBS"
WELIBS="$(pkg-config --libs Qt6WebEngineWidgets)"
PDFLIBS="$(pkg-config --libs Qt6PdfWidgets)"
sed -i "s|CPPGO_CXX|$COMMON_CXX|; s|CPPGO_LIBS|$QTLIBS|"  "$ROOT/qt/gen_support.go"
sed -i "s|CPPGO_CXX|$COMMON_CXX|; s|CPPGO_LIBS|$KF6LIBS|" "$ROOT/kf6/gen_support.go"
sed -i "s|CPPGO_CXX|$COMMON_CXX|; s|CPPGO_LIBS|$WELIBS|" "$ROOT/webengine/gen_support.go"
sed -i "s|CPPGO_CXX|$COMMON_CXX|; s|CPPGO_LIBS|$PDFLIBS|" "$ROOT/pdf/gen_support.go"

# (1) (REMOVED) The old gcc PCH precompile (cppgo_pch.h.gch) was for cgo-compiled shims, which no longer
# exist under v2 — shims live in cshim/ and are compiled by the toolexec wrapper (clang driver) against a
# full-header PCH it builds. Precompiling cppgo_pch.h here was dead work (4× per regen).

# GO-COMPILE flags — the single biggest lever (the Go compile is the serial, memory-bound long pole; it
# peaked at 4.9 GB and was the original OOM). -N -l disable optimization + inlining: the Go-side analog of
# the shims' -O0. Thin cgo wrappers gain ~nothing at runtime from Go optimization (the cost is the cgo
# boundary), so it's near-free perf-wise while cutting Go compile time AND peak memory on this
# pathologically large generated package.
GCF='-gcflags=-N -l'
# NOTE on parallelism: kf6/webengine/pdf import ONLY qt (not each other), so they COULD build in parallel
# (`go build $GCF ./kf6 ./webengine ./pdf`). But each Go compile of these huge packages is multi-GB, and
# this Deck has ~7 GB usable — 3 concurrent compiles would re-introduce the OOM. So build SEQUENTIALLY
# here; the -N -l memory reduction is the safe win. (Enable the parallel form only on a high-RAM machine.)
flt() { grep -vE "deprecated|note:|warning|In (function|member|instantiation|file)|^ +\||~~|^ +[0-9]+ \|" || true; }
# --- v2 pipeline: leg 1 (forked cgo, discovery-skip) + leg 3 (out-of-package shims -> libshim.a) ---
# leg 1: the forked cgo lives as additive cmd/cppgocgo in GOROOT (spliced by restore_cppgocgo.sh).
# Build it to a binary literally named `cgo` so `go build -toolexec` accepts it.
REPO=$(pwd)
# Sync the tracked fork (cgofork/cppgocgo) into GOROOT, then build it — so a wiped GOROOT self-heals.
echo ">> build forked cgo (leg 1)"; bash cgofork/restore_cppgocgo.sh; mkdir -p /tmp/cgofork; go build -o /tmp/cgofork/cgo cmd/cppgocgo
# leg 3: the toolexec wrapper compiles cshim/*.cpp -> cshim/libshim.a with the clang DRIVER (clang++ -c,
# batched -P W) against a full-header PCH it builds once. (The old in-process cppgoshim/cppgoshim_split
# LibTooling tools were REMOVED: EmitObjAction dropped ODR-used inline closures -> non-self-contained
# libshim.a; the driver emits them as weak defs. See REGEN.md §3.)
export CPPGO_CGO=/tmp/cgofork/cgo
export CPPGO_W="${CPPGO_W:-8}"
# Clang flags for the shim parse/compile: mirror the cgo CXX compile (Qt -I + std + -O0 -fPIC) + clang's
# own builtin-header resource dir (the driver needs it explicitly). Wrapper adds -I<pkg> + -include _cgo_export.h.
export CPPGOSHIM_CXX="-std=c++17 -O0 -g0 -fPIC $COMMON_CXX -resource-dir $(clang -print-resource-dir)"
WRAP="$REPO/cgofork/cppgo_toolexec.sh"; chmod +x "$WRAP"
echo ">> v2: forked-cgo=$CPPGO_CGO  W=$CPPGO_W  toolexec=$WRAP"

cd "$ROOT"
# Build ALL packages in ONE go build so the shared qt dependency is built exactly ONCE (the separate
# per-package builds re-ran qt's whole cgo/shim/gc when kf6/webengine/pdf — which import cppgoqt/qt —
# were built, because -toolexec defeated the build cache). kf6/webengine/pdf don't import each other, so
# go build may compile them concurrently after qt; if that peaks memory, drop to `-p 2`.
echo ">> build all packages (-N -l, v2, single graph — qt built once)"
go build "$GCF" -toolexec="$WRAP" ./qt ./kf6 ./webengine ./pdf 2>&1 | flt; echo "ALL-EXIT=${PIPESTATUS[0]}"
# libshim.a is a toolexec BY-PRODUCT of the cgo step: with a warm Go build cache the cgo step can be
# skipped entirely, leaving the archive missing/stale. The generated gen_shimstamp.go makes shim CHANGES
# a cache miss; this catches the remaining case (fresh tree + warm cache) by forcing the package build.
for p in qt kf6 webengine pdf; do
	if [ ! -f "$ROOT/$p/cshim/libshim.a" ]; then
		echo ">> $p/cshim/libshim.a missing — forcing the cgo step (go build -a $p)"
		go build "$GCF" -toolexec="$WRAP" -a ./$p 2>&1 | flt; echo "$p-FORCE-EXIT=${PIPESTATUS[0]}"
	fi
done
echo ">> ALL DONE"
