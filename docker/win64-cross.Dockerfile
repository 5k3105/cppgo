# cppgo win64 cross-build container — derived from the miqt project's win64-cross recipe
# (docker/win64-cross-go1.24-qt6.10-dynamic.Dockerfile): Debian + mingw-w64 g++ (compiles the
# shim DLL) + the OFFICIAL Qt win64_mingw payload via aqtinstall (Windows headers/libs/.pc files).
#
# cppgo additions:
#   - clang/LLVM dev: the cppir2 frontend runs as a HOST tool but parses the WINDOWS Qt headers
#     with -target x86_64-w64-mingw32, so clang resolves every type at Windows (LLP64) widths.
#   - wine: best-effort RUNTIME verification of the produced .exe + shim DLL, still on Linux.
#   - no Go baked in: mount a host Go toolchain at /usr/local/go (the generated module wants a
#     recent `go` directive; distro Go lags).
#
# Build:  podman build -t cppgo-win64-cross -f docker/win64-cross.Dockerfile docker/
# Use:    see windows.sh (cross mode) / WINDOWS.md
FROM debian:13

RUN DEBIAN_FRONTEND=noninteractive apt-get update && \
    apt-get install -qyy build-essential python3-pip python3-venv ca-certificates \
      gcc-mingw-w64-x86-64 g++-mingw-w64-x86-64 mingw-w64-x86-64-dev \
      clang llvm-dev libclang-cpp-dev libclang-dev pkg-config && \
    apt-get clean && \
    python3 -m venv /aqtinstall && \
    /aqtinstall/bin/pip3 install aqtinstall && \
    rm -r /root/.cache

# The official Qt for Windows (mingw) payload. The shipped .pc files carry the Qt CI's build
# prefix; rewrite it to the install location (same fixup as miqt).
RUN mkdir /qt && \
    cd /qt && \
    /aqtinstall/bin/aqt install-qt windows desktop 6.10.0 win64_mingw && \
    find . -name '*.pc' -exec sed -i -re 's~/Users/qt/work/install~/qt/6.10.0/mingw_64~g' {} \;

# wine (separate layer: optional, and its package set churns more than the toolchain above)
RUN DEBIAN_FRONTEND=noninteractive apt-get update && \
    apt-get install -qyy wine wine64 && \
    apt-get clean

ENV CXX=x86_64-w64-mingw32-g++ \
    CC=x86_64-w64-mingw32-gcc \
    PKG_CONFIG_PATH=/qt/6.10.0/mingw_64/lib/pkgconfig \
    PATH=/usr/local/go/bin:$PATH \
    GOFLAGS=-buildvcs=false
