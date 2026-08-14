#!/usr/bin/env bash
# Bind a curated set of KF6 frameworks (the KTextEditor / Kate stack) into ONE package, layered on the
# Qt Core/Gui/Widgets tier. Unlike a Qt module, KF6 has no single umbrella header — it's ~40 independent
# frameworks — so we synthesize an umbrella from each chosen framework's CamelCase forward headers.
# QML/Quick headers are skipped on purpose: we don't bind QML, and a QML header's <QtQmlIntegration>
# include is a FATAL parse error that corrupts clang's downstream type resolution (turning unrelated
# QPair/QList returns into `int`). Requires a Linux env with clang/LLVM, Qt 6 dev headers, and Go (see BUILDING.md).
set -euo pipefail
cd "$(dirname "$0")"

FWS="KCoreAddons KGuiAddons KWidgetsAddons KConfigCore KConfigGui KConfigWidgets KI18n KXmlGui KParts KSyntaxHighlighting KTextEditor"

echo ">> build cppir2"
clang++ -std=c++17 -fno-rtti frontend/cppir2.cpp -o ./cppir2 -lclang-cpp -lLLVM 2>&1 | tail -3

echo ">> synthesize KF6 umbrella (forward headers, minus QML/Quick)"
> /tmp/umbrella_KF6.h
for fw in $FWS; do
  find "/usr/include/KF6/$fw" -maxdepth 2 -type f -regextype posix-egrep -regex ".*/[A-Z][A-Za-z0-9]*" \
       ! -iname "*Qml*" ! -iname "*Quick*" 2>/dev/null
done | sort | while read -r h; do echo "#include \"$h\""; done >> /tmp/umbrella_KF6.h

KF6INC=$(for d in /usr/include/KF6/*/; do printf -- "-I%s " "$d"; done)
CF="$(pkg-config --cflags Qt6Widgets) $KF6INC"
echo ">> extract IR (filter=KF6,QtCore,QtGui,QtWidgets)"
./cppir2 /tmp/umbrella_KF6.h "KF6,QtCore,QtGui,QtWidgets" $CF > /tmp/KF6.json
python3 -c "import json;d=json.load(open('/tmp/KF6.json'));print('  records=%d enums=%d'%(len(d['records']),len(d['enums'])))"

OUT=/tmp/KF6_out
rm -rf "$OUT"; mkdir -p "$OUT"
echo ">> gen all"
go run ./cmd/gen "$OUT" all /tmp/KF6.json

# COMMON compile flags (identical to the Qt tiers -> shared cgo cache); per-tier KF6 + Qt link libs.
COMMON_CXX="$(pkg-config --cflags Qt6Widgets) $KF6INC"
KF6LIBS="-lKF6TextEditor -lKF6Parts -lKF6CoreAddons -lKF6WidgetsAddons -lKF6GuiAddons -lKF6ConfigCore -lKF6ConfigGui -lKF6ConfigWidgets -lKF6I18n -lKF6XmlGui -lKF6SyntaxHighlighting -lKF6Completion -lKF6KIOCore $(pkg-config --libs Qt6Widgets)"
sed -i "s|CPPGO_CXX|$COMMON_CXX|; s|CPPGO_LIBS|$KF6LIBS|" "$OUT"/*.go

echo ">> build KF6 package"
( cd "$OUT" && go build . 2>&1 | grep -vE "deprecated|note:|warning:|In (function|member|instantiation|file)|^ +\||~~|^ +[0-9]+ \|" | head -40 )
echo ">> done"
