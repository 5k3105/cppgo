#!/usr/bin/env bash
# Multi-package build: the base "qt" package (full Core+Gui+Widgets) plus a dependent module package
# (kf6 / webengine / pdf) that IMPORTS it. Each package is its own buildable unit in one Go module
# (cppgoqt) — so the Go compiler never holds the whole surface at once (which OOMs), and cross-module
# references resolve by USR to the base package's types (qt.QWidget, ...).
# Usage: ./mp.sh kf6 | webengine | pdf      (needs Qt 6 + clang dev env; see BUILDING.md)
set -euo pipefail
cd "$(dirname "$0")"
DEP="${1:-kf6}"
ROOT=/tmp/cppgoqt
KF6INC=$(for d in /usr/include/KF6/*/; do printf -- "-I%s " "$d"; done)
COMMON_CXX="$(pkg-config --cflags Qt6Widgets) $KF6INC"
QTLIBS="$(pkg-config --libs Qt6Widgets)"

echo ">> build cppir2"
clang++ -std=c++17 -fno-rtti frontend/cppir2.cpp -o ./cppir2 -lclang-cpp -lLLVM 2>&1 | tail -2

# --- base "qt": full Core+Gui+Widgets ---
echo ">> extract qt base (Core+Gui+Widgets)"
echo "#include <QtWidgets/QtWidgets>" > /tmp/umbrella_qt.h
./cppir2 /tmp/umbrella_qt.h "QtCore,QtGui,QtWidgets" $(pkg-config --cflags Qt6Widgets) > /tmp/qtbase.json

# --- dependent module extraction (umbrella + filter + link libs) ---
case "$DEP" in
  kf6)
    FWS="KCoreAddons KGuiAddons KWidgetsAddons KConfigCore KConfigGui KConfigWidgets KI18n KXmlGui KParts KSyntaxHighlighting KTextEditor"
    > /tmp/umbrella_dep.h
    for fw in $FWS; do
      find "/usr/include/KF6/$fw" -maxdepth 2 -type f -regextype posix-egrep -regex ".*/[A-Z][A-Za-z0-9]*" ! -iname "*Qml*" ! -iname "*Quick*" 2>/dev/null
    done | sort | while read -r h; do echo "#include \"$h\""; done >> /tmp/umbrella_dep.h
    FILTER="KF6,QtCore,QtGui,QtWidgets"; CF="$COMMON_CXX"
    DEPLIBS="-lKF6TextEditor -lKF6Parts -lKF6CoreAddons -lKF6Crash -lKF6WidgetsAddons -lKF6GuiAddons -lKF6ConfigCore -lKF6ConfigGui -lKF6ConfigWidgets -lKF6I18n -lKF6XmlGui -lKF6SyntaxHighlighting -lKF6Completion -lKF6KIOCore $QTLIBS" ;;
  webengine)
    echo "#include <QtWebEngineWidgets/QtWebEngineWidgets>" > /tmp/umbrella_dep.h
    FILTER="QtCore,QtGui,QtWidgets,QtWebEngineCore,QtWebEngineWidgets"; CF="$(pkg-config --cflags Qt6WebEngineWidgets)"
    DEPLIBS="$(pkg-config --libs Qt6WebEngineWidgets)" ;;
  pdf)
    echo "#include <QtPdfWidgets/QtPdfWidgets>" > /tmp/umbrella_dep.h
    FILTER="QtCore,QtGui,QtWidgets,QtPdf"; CF="$(pkg-config --cflags Qt6PdfWidgets)"
    DEPLIBS="$(pkg-config --libs Qt6PdfWidgets)" ;;
  *) echo "unknown dep $DEP"; exit 1 ;;
esac
echo ">> extract $DEP"
./cppir2 /tmp/umbrella_dep.h "$FILTER" $CF > /tmp/dep.json

# --- module + per-package emit ---
rm -rf "$ROOT"; mkdir -p "$ROOT"
printf 'module cppgoqt\n\ngo 1.26\n' > "$ROOT/go.mod"
echo ">> build gen"; go build -o ./cppgogen ./cmd/gen # prebuilt binary (no per-call `go run`); NOT ./gen (collides with gen/ dir)
echo ">> gen qt"
./cppgogen "$ROOT/qt"  qt   -            all /tmp/qtbase.json
echo ">> gen $DEP"
./cppgogen "$ROOT/$DEP" "$DEP" cppgoqt/qt all /tmp/dep.json

# cgo flags: common compile flags everywhere; per-package link libs.
sed -i "s|CPPGO_CXX|$COMMON_CXX|; s|CPPGO_LIBS|$QTLIBS|"  "$ROOT/qt/gen_support.go"
sed -i "s|CPPGO_CXX|$COMMON_CXX|; s|CPPGO_LIBS|$DEPLIBS|" "$ROOT/$DEP/gen_support.go"

# Build-speed: ccache (if present) + precompile each package's PCH (best-effort; see all.sh).
if command -v ccache >/dev/null; then
	export CC="ccache gcc" CXX="ccache g++"
	export CCACHE_MAXSIZE="${CCACHE_MAXSIZE:-25G}" CCACHE_DEPEND=1
	# CRITICAL for PCH: ccache won't cache PCH-using compiles without these sloppiness flags.
	export CCACHE_SLOPPINESS="pch_defines,time_macros,include_file_mtime,include_file_ctime"
fi
CXXBIN="${CXX:-g++}" # unquoted on use so "ccache g++" word-splits into program + arg
for p in qt "$DEP"; do
  $CXXBIN -x c++-header -std=c++17 -O0 -g0 -fPIC $COMMON_CXX -I"$ROOT/$p" "$ROOT/$p/cppgo_pch.h" -o "$ROOT/$p/cppgo_pch.h.gch" 2>/dev/null || true
done

# -gcflags=-N -l: disable Go optimization + inlining — the Go compile is the serial, memory-bound long
# pole (the 4.9 GB peak / original OOM); thin cgo wrappers gain ~nothing at runtime, so it's near-free.
GCF='-gcflags=-N -l'
echo ">> build qt (-N -l)"
( cd "$ROOT" && go build "$GCF" ./qt 2>&1 | grep -vE "deprecated|note:|warning:|In (function|member|instantiation|file)|^ +\||~~|^ +[0-9]+ \|" | head -30 )
echo ">> build $DEP (imports qt, -N -l)"
( cd "$ROOT" && go build "$GCF" ./$DEP 2>&1 | grep -vE "deprecated|note:|warning:|In (function|member|instantiation|file)|^ +\||~~|^ +[0-9]+ \|" | head -40 )
echo ">> done"
