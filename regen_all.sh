#!/usr/bin/env bash
# Full regen with FRESH JSON extraction. Use after a frontend (cppir2.cpp) change, which invalidates ALL
# cached JSON (all.sh only re-extracts pdf). Builds cppir2, re-extracts qt/webengine/pdf/kf6 IR, then
# delegates gen+build of all four packages to all.sh. Requires a Linux env with clang/LLVM, Qt 6 dev headers, and Go (see BUILDING.md).
set -uo pipefail
cd "$(dirname "$0")"
export GOTMPDIR="${GOTMPDIR:-$HOME/.cache/gobuild-tmp}"
export TMPDIR="$GOTMPDIR"
mkdir -p "$GOTMPDIR"

echo ">> build cppir2"
clang++ -std=c++17 -fno-rtti frontend/cppir2.cpp -o ./cppir2 -lclang-cpp -lLLVM 2>&1 | tail -3
[ -x ./cppir2 ] || { echo "cppir2 build FAILED"; exit 1; }

KF6INC=$(for d in /usr/include/KF6/*/; do printf -- "-I%s " "$d"; done)
COMMON_CXX="$(pkg-config --cflags Qt6Widgets) $KF6INC"

echo ">> extract qt base (Core+Gui+Widgets)"
echo "#include <QtWidgets/QtWidgets>" > /tmp/umbrella_qt.h
./cppir2 /tmp/umbrella_qt.h "QtCore,QtGui,QtWidgets" $(pkg-config --cflags Qt6Widgets) > /tmp/qtbase.json

echo ">> extract webengine"
echo "#include <QtWebEngineWidgets/QtWebEngineWidgets>" > /tmp/umbrella_dep.h
./cppir2 /tmp/umbrella_dep.h "QtCore,QtGui,QtWidgets,QtWebEngineCore,QtWebEngineWidgets" \
  $(pkg-config --cflags Qt6WebEngineWidgets) > /tmp/dep.json

echo ">> extract pdf"
echo "#include <QtPdfWidgets/QtPdfWidgets>" > /tmp/umbrella_pdf.h
./cppir2 /tmp/umbrella_pdf.h "QtCore,QtGui,QtWidgets,QtPdf" \
  $(pkg-config --cflags Qt6PdfWidgets) > /tmp/pdf.json

echo ">> synthesize + extract KF6"
FWS="KCoreAddons KGuiAddons KWidgetsAddons KConfigCore KConfigGui KConfigWidgets KI18n KXmlGui KParts KSyntaxHighlighting KTextEditor"
> /tmp/umbrella_KF6.h
for fw in $FWS; do
  find "/usr/include/KF6/$fw" -maxdepth 2 -type f -regextype posix-egrep -regex ".*/[A-Z][A-Za-z0-9]*" ! -iname "*Qml*" ! -iname "*Quick*" 2>/dev/null
done | sort | while read -r h; do echo "#include \"$h\""; done >> /tmp/umbrella_KF6.h
./cppir2 /tmp/umbrella_KF6.h "KF6,QtCore,QtGui,QtWidgets" $COMMON_CXX > /tmp/KF6.json

echo ">> JSON extracted:"; ls -la /tmp/qtbase.json /tmp/dep.json /tmp/pdf.json /tmp/KF6.json
echo ">> new-field smoke check (qtbase):"
python3 - <<'PY'
import json
d = json.load(open('/tmp/qtbase.json'))
recs = d['records']
pure = sum(1 for r in recs for m in r.get('methods',[])+r.get('pureVirtuals',[]) if m.get('pure'))
meta = sum(1 for r in recs if r.get('metaObject'))
scoped = sum(1 for e in d['enums'] if e.get('scoped'))
qtg = sum(1 for r in recs for m in r.get('methods',[]) if m['name']=='qt_check_for_QGADGET_macro')
print(f"  records={len(recs)} methods-pure={pure} metaObject-classes={meta} scoped-enums={scoped} qt_check_leaks={qtg}")
assert qtg == 0, "qt_check_for_QGADGET_macro STILL bound!"
assert pure > 0 and meta > 0 and scoped >= 0, "new fields missing"
print("  OK: new fields present, qt_check filtered")
PY

echo ">> delegate to all.sh (gen + build all four packages)"
bash all.sh
echo ">> REGEN_ALL DONE"
