#!/usr/bin/env bash
# Bind a WHOLE Qt module: parse its umbrella header once, emit every bound record, build the package.
# Usage: ./module.sh QtCore | QtGui | QtWidgets    (needs Qt 6 + clang dev env; see BUILDING.md)
set -euo pipefail
cd "$(dirname "$0")"
MOD="${1:-QtCore}"

# A module is emitted together with its DEPENDENCY TIER into ONE package (miqt's model): a QtWidgets
# method that takes a QtCore QObject must see QObject as a bound type, so we keep every tier's decls
# (FILTER) and link every tier's libs (PKGS). Cross-module refs then resolve within one bound set.
case "$MOD" in
  QtCore)      PKGS="Qt6Core";              FILTER="QtCore";                                          UMB="QtCore/QtCore" ;;
  QtGui)       PKGS="Qt6Gui";               FILTER="QtCore,QtGui";                                    UMB="QtGui/QtGui" ;;
  QtWidgets)   PKGS="Qt6Widgets";           FILTER="QtCore,QtGui,QtWidgets";                          UMB="QtWidgets/QtWidgets" ;;
  QtPdf)       PKGS="Qt6PdfWidgets";        FILTER="QtCore,QtGui,QtWidgets,QtPdf";                    UMB="QtPdfWidgets/QtPdfWidgets" ;;
  QtWebEngine) PKGS="Qt6WebEngineWidgets";  FILTER="QtCore,QtGui,QtWidgets,QtWebEngineCore,QtWebEngineWidgets"; UMB="QtWebEngineWidgets/QtWebEngineWidgets" ;;
  *)           PKGS="Qt6Core";              FILTER="$MOD";                                            UMB="$MOD/$MOD" ;;
esac
CF=$(pkg-config --cflags $PKGS)

echo ">> build cppir2"
clang++ -std=c++17 -fno-rtti frontend/cppir2.cpp -o ./cppir2 -lclang-cpp -lLLVM 2>&1 | tail -3

echo "#include <$UMB>" > /tmp/umbrella_$MOD.h
echo ">> extract IR for $MOD (filter=$FILTER)"
./cppir2 /tmp/umbrella_$MOD.h "$FILTER" $CF > /tmp/$MOD.json
python3 -c "import json;d=json.load(open('/tmp/$MOD.json'));print('  records=%d enums=%d'%(len(d['records']),len(d['enums'])))"

OUT=/tmp/${MOD}_out
rm -rf "$OUT"; mkdir -p "$OUT"
echo ">> gen all"
go run ./cmd/gen "$OUT" all /tmp/$MOD.json
# COMMON compile flags — identical for EVERY tier so the cgo build cache is shared across modules
# (a Widgets shim compiles to the same object whether built under QtWidgets, QtPdf or QtWebEngine).
# Qt headers resolve through -I/usr/include/qt6 regardless of module; KF6 framework dirs are added
# unconditionally (harmless/unused for Qt-only tiers, but keeps the flag string identical everywhere).
KF6INC=$(for d in /usr/include/KF6/*/; do printf -- "-I%s " "$d"; done)
COMMON_CXX="$(pkg-config --cflags Qt6Widgets) $KF6INC"
# Per-tier LINK libraries only (affects linking, not the per-file compile cache).
LIBS="$(pkg-config --libs $PKGS)"
sed -i "s|CPPGO_CXX|$COMMON_CXX|; s|CPPGO_LIBS|$LIBS|" "$OUT"/*.go

echo ">> build $MOD package"
( cd "$OUT" && go build . 2>&1 | grep -vE "deprecated|note:|warning:|In (function|member|instantiation|file)|^ +\||~~|^ +[0-9]+ \|" | head -60 )
echo ">> build exit ${PIPESTATUS:-0}"
