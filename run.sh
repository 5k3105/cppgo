#!/usr/bin/env bash
# Build the LibTooling frontend, extract IR for the marquee headers (the ones that broke
# miqt), and run the bindability demo. Requires a Linux env with libclang/LLVM,
# Qt 6 dev headers + Go (see BUILDING.md).
set -euo pipefail
cd "$(dirname "$0")"

CF=$(pkg-config --cflags Qt6Core)
QH=$(pkg-config --variable=includedir Qt6Core)

echo ">> building LibTooling frontend (cppir2)..."
clang++ -std=c++17 -fno-rtti frontend/cppir2.cpp -o ./cppir2 -lclang-cpp -lLLVM

mkdir -p out
echo ">> extracting IR (full type fidelity; frontend auto-passes -resource-dir)..."
./cppir2 "$QH/QtCore/qstringconverter.h"   "converter"         $CF > out/stringconverter.json
./cppir2 "$QH/QtCore/qbytearrayview.h"     "bytearrayview"     $CF > out/bytearrayview.json
./cppir2 "$QH/QtCore/qcryptographichash.h" "cryptographichash" $CF > out/cryptographichash.json
./cppir2 "$QH/QtCore/qbytearray.h"         "qbytearray"        $CF > out/qbytearray.json

echo ">> bindability report:"
go run ./cmd/demo out/stringconverter.json out/bytearrayview.json out/cryptographichash.json
