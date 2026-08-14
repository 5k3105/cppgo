#!/usr/bin/env bash
# Build libshim_<pkg>.so for a purego-backend package. The purego Go
# bindings dlopen this at runtime (CPPGO_LIB_<PKG>); it bundles the cshim C++ method shims +
# the per-shape C->Go dispatch table (gen_purego_cb.cpp). Requires a Linux env with clang/LLVM, Qt 6 dev headers, and Go (see BUILDING.md).
#
#   ./buildso.sh <pkgdir> <pkg> [pkg-config packages]
#
# e.g. ./buildso.sh /tmp/cppgoqt-pg/qt qt
#      ./buildso.sh /tmp/cppgoqt-pg/webengine webengine Qt6WebEngineWidgets
#      ./buildso.sh /tmp/cppgoqt-pg/pdf pdf Qt6PdfWidgets
#      ./buildso.sh /tmp/cppgoqt-pg/kf6 kf6      (KF6 ships no pkg-config; handled below)
set -euo pipefail
pkgdir="$1"; pkg="$2"; pkgs="Qt6Widgets ${3:-}"
libs="$(pkg-config --libs $pkgs)"
cxx="-std=c++17 -O0 -g0 -fPIC $(pkg-config --cflags $pkgs) -resource-dir $(clang -print-resource-dir)"
if [ "$pkg" = kf6 ]; then
	# KDE Frameworks ship CMake configs, not pkg-config — add their include dirs + libs directly
	# (the same set the cgo pipeline links in all.sh).
	cxx="$cxx $(for d in /usr/include/KF6/*/; do printf -- "-I%s " "$d"; done)"
	libs="$libs -lKF6TextEditor -lKF6Parts -lKF6CoreAddons -lKF6Crash -lKF6WidgetsAddons -lKF6GuiAddons -lKF6ConfigCore -lKF6ConfigGui -lKF6ConfigWidgets -lKF6I18n -lKF6XmlGui -lKF6SyntaxHighlighting -lKF6Completion -lKF6KIOCore"
fi
cshim="$pkgdir/cshim"
out="$(cd "$pkgdir/.." && pwd)/libshim_${pkg}.so"

echo ">> PCH"
pch="$cshim/cppgo_pch.pch"
clang++ -x c++-header $cxx -I"$pkgdir" "$pkgdir/cppgo_pch.h" -o "$pch"

echo ">> compile units (-P ${CPPGO_W:-4})"
scd=$(mktemp -d)
ls "$cshim"/*.cpp | xargs -P "${CPPGO_W:-4}" -I{} sh -c \
  'clang++ '"$cxx"' -include-pch '"$pch"' -I"'"$pkgdir"'" -c "$1" -o "'"$scd"'/$(basename "$1").o" || { echo "FAIL: $1"; exit 1; }' _ {}

echo ">> link $out"
clang++ -shared -o "$out" "$scd"/*.o $libs -lstdc++ -lm
echo "built $out ($(ls "$scd"/*.o | wc -l) objects, $(du -h "$out" | cut -f1))"
rm -rf "$scd"
