#!/usr/bin/env bash
# windows.sh — generate + build the cgo-free (purego) Windows bindings, LLP64 data model.
#
# TWO WAYS TO RUN IT (mode auto-detected):
#
#   A. CROSS from Linux — recommended; no Windows machine needed.
#      Container recipe derived from the miqt project's win64-cross image:
#        podman build -t cppgo-win64-cross -f docker/win64-cross.Dockerfile docker/
#        podman run --rm -v "$PWD":/src -v <host-go-root>:/usr/local/go -w /src \
#            cppgo-win64-cross bash windows.sh
#      The frontend runs as a HOST tool but parses the WINDOWS Qt headers with
#      -target x86_64-w64-mingw32, so clang resolves every type at Windows (LLP64) widths;
#      mingw g++ compiles the shim DLL; the Go side builds GOOS=windows CGO_ENABLED=0.
#
#   B. NATIVE on Windows, in an MSYS2 CLANG64 shell:
#        pacman -S mingw-w64-clang-x86_64-{clang,qt6-base,pkgconf,go}
#
# Afterwards: build your app with `CGO_ENABLED=0 GOOS=windows go build .` and ship
# shim_qt.dll beside the exe (or point CPPGO_LIB_QT at it). Cross builds can be runtime-
# smoked under wine (see WINDOWS.md).
set -euo pipefail
cd "$(dirname "$0")"
ROOT="${ROOT:-/tmp/cppgoqt-win}"
TMP="${TMPDIR:-/tmp}"

CROSS=0
if [ "$(uname -s)" = Linux ] && command -v x86_64-w64-mingw32-g++ >/dev/null; then
	CROSS=1
	SHIM_CXX=x86_64-w64-mingw32-g++
	TARGET_FLAGS="--target=x86_64-w64-mingw32"
	echo ">> mode: CROSS (Linux host -> win64 target)"
else
	SHIM_CXX=clang++
	TARGET_FLAGS=""
	echo ">> mode: NATIVE (MSYS2)"
fi

echo ">> [1/5] build frontend (host tool)"
# Debian keeps the clang/LLVM dev headers under a versioned prefix (/usr/lib/llvm-N); Arch and
# MSYS2 keep them on the default paths. llvm-config bridges both.
LLVM_INC="$(llvm-config --includedir 2>/dev/null || true)"
LLVM_LIB="$(llvm-config --libdir 2>/dev/null || true)"
clang++ -std=c++17 -fno-rtti ${LLVM_INC:+-I"$LLVM_INC"} ${LLVM_LIB:+-L"$LLVM_LIB"} \
	frontend/cppir2.cpp -o ./cppir2 -lclang-cpp -lLLVM

echo ">> [2/5] extract IR from the WINDOWS Qt headers (LLP64 widths resolve here)"
echo "#include <QtWidgets/QtWidgets>" > "$TMP/umbrella_qt.h"
./cppir2 "$TMP/umbrella_qt.h" "QtCore,QtGui,QtWidgets" \
	$TARGET_FLAGS $(pkg-config --cflags Qt6Widgets) > "$TMP/qtbase-win.json"
python3 -c "import json;d=json.load(open('$TMP/qtbase-win.json'));print('  records=%d enums=%d'%(len(d['records']),len(d['enums'])))"

echo ">> [3/5] generate (purego backend, llp64)"
go build -o ./cppgogen ./cmd/gen
rm -rf "$ROOT"; mkdir -p "$ROOT"
printf 'module cppgoqt\n\ngo 1.26\n' > "$ROOT/go.mod"
CPPGO_BACKEND=purego CPPGO_DATAMODEL=llp64 \
	./cppgogen "$ROOT/qt" qt - all "$TMP/qtbase-win.json"
( cd "$ROOT" && go mod tidy )

echo ">> [4/5] compile shims -> shim_qt.dll ($SHIM_CXX)"
cxx="-std=c++17 -O0 -g0 $(pkg-config --cflags Qt6Widgets)"
scd=$(mktemp -d)
ls "$ROOT"/qt/cshim/*.cpp | xargs -P "${CPPGO_W:-4}" -I{} sh -c \
	''"$SHIM_CXX"' '"$cxx"' -I"'"$ROOT"'/qt" -include "'"$ROOT"'/qt/cppgo_pch.h" -c "$1" -o "'"$scd"'/$(basename "$1").o" || { echo "FAIL: $1"; exit 1; }' _ {}
# -static-libgcc/-static-libstdc++: embed the mingw runtimes so the DLL needs only the Qt DLLs.
$SHIM_CXX -shared -static-libgcc -static-libstdc++ -o "$ROOT/shim_qt.dll" "$scd"/*.o \
	$(pkg-config --libs Qt6Widgets)
rm -rf "$scd"
echo "built $ROOT/shim_qt.dll ($(du -h "$ROOT/shim_qt.dll" | cut -f1))"

echo ">> [5/5] sanity: the generated package compiles CGO_ENABLED=0 GOOS=windows"
( cd "$ROOT" && CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./qt )

echo ">> DONE."
