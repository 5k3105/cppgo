#!/usr/bin/env bash
# Focused qt-only re-gen + build (gen.go iteration; frontend/JSON unchanged). Captures ALL C++ errors.
set -uo pipefail
cd "$(dirname "$0")"
export GOTMPDIR="${GOTMPDIR:-$HOME/.cache/gobuild-tmp}"
export TMPDIR="$GOTMPDIR"
export CPPGO_UNITS=0   # per-class shims, matching all.sh; also rm the dir so no STALE per-class files linger
ROOT=/tmp/cppgoqt
KF6INC=$(for d in /usr/include/KF6/*/; do printf -- "-I%s " "$d"; done)
COMMON_CXX="$(pkg-config --cflags Qt6Widgets) $KF6INC"
QTLIBS="$(pkg-config --libs Qt6Widgets)"
if command -v ccache >/dev/null; then export CC="ccache gcc" CXX="ccache g++" CCACHE_MAXSIZE=25G CCACHE_DEPEND=1 CCACHE_SLOPPINESS="pch_defines,time_macros,include_file_mtime,include_file_ctime"; fi
echo ">> build gen"; go build -o ./cppgogen ./cmd/gen || exit 1
echo ">> gen qt"; rm -rf "$ROOT/qt"; ./cppgogen "$ROOT/qt" qt - all /tmp/qtbase.json
sed -i "s|CPPGO_CXX|$COMMON_CXX|; s|CPPGO_LIBS|$QTLIBS|" "$ROOT/qt/gen_support.go"
${CXX:-g++} -x c++-header -std=c++17 -O0 -g0 -fPIC $COMMON_CXX -I"$ROOT/qt" "$ROOT/qt/cppgo_pch.h" -o "$ROOT/qt/cppgo_pch.h.gch" 2>/dev/null || true
echo ">> build qt"
( cd "$ROOT" && go build -p 4 -gcflags="-N -l" ./qt 2>&1 | grep -E "error:|\.go:[0-9]+:[0-9]+:|undefined reference" | grep -vE "deprecated|note:|warning:" | sort -u | head -50 )
echo "QT-EXIT=${PIPESTATUS[0]}"
echo ">> qtonly DONE"
