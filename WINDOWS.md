# Windows support — status

The Windows port targets the **purego (cgo-free) backend**: a consumer app builds with
`CGO_ENABLED=0 GOOS=windows` and loads `shim_<pkg>.dll` at runtime. The cgo backend's v2
pipeline (forked cgo in GOROOT, toolexec wrapper, `libshim.a`) is POSIX-shaped and intentionally
not ported — the purego backend exists precisely to avoid it.

## Done (verified on Linux, cross-targeting Windows)

- **LLP64 data model** — `CPPGO_DATAMODEL=llp64` switches the width-ambiguous C types; pinned by
  unit tests (`gen/emit_syntax_test.go: TestDataModelWidths`):

  | C type | lp64 (Linux/macOS) | llp64 (Windows) |
  |---|---|---|
  | `long` | Go `int` (8B) | Go `int32` |
  | `unsigned long` | Go `uint` (8B) | Go `uint32` |
  | `wchar_t` | `int` / Go `int32` | `unsigned short` / Go `uint16` |

  Everything else (`int`, `long long`, pointers, `size_t`-via-canonical) is identical. The IR
  must be extracted from the **Windows** Qt headers (clang resolves `size_t`, `qsizetype`, `WId`
  per target); the switch covers only the spelled-`long`/`wchar_t` residue.
- **Per-GOOS loader** — the generated support is split into `gen_purego_unix.go`
  (`purego.Dlopen`, RTLD_LOCAL, `libshim_<pkg>.so`) and `gen_purego_windows.go`
  (`syscall.LoadLibrary`/`GetProcAddress`, `shim_<pkg>.dll`). `purego`'s
  `RegisterLibFunc`/`SyscallN`/`NewCallback` are cross-platform; only its `Dlopen` was unix-only.
  `$CPPGO_LIB_<PKG>` overrides the path on both.
- **Shim-exported allocator** — the shim library now exports `cppgo_malloc`/`cppgo_free`
  (`cshim/cppgo_alloc.cpp`, generated) and the Go side binds those, so buffers are freed by the
  exact C runtime that allocated them. This removed the `libc.so.6` hardcode on Linux too.
- **Cross-compile proof** — all four packages (qt/kf6/webengine/pdf), generated with
  `CPPGO_BACKEND=purego CPPGO_DATAMODEL=llp64`, compile with
  `CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build` — zero errors. (Compile-shape proof against
  Linux-extracted IR; the real thing re-extracts on Windows.)
- **`windows.sh`** — the 5-stage driver (frontend → Windows IR → llp64 gen → `shim_qt.dll` →
  `GOOS=windows` build). Stages 3 and 5 are the ones exercised above.

## Verified end to end — cross from Linux, runtime under wine

The whole pipeline runs on a Linux machine via `docker/win64-cross.Dockerfile` (derived from
the miqt project's win64-cross recipe: Debian + mingw-w64 g++ + the official Qt `win64_mingw`
payload via aqtinstall, plus clang/LLVM for the frontend and wine for runtime verification):

```sh
podman build -t cppgo-win64-cross -f docker/win64-cross.Dockerfile docker/
podman run --rm -v "$PWD":/src -v <host-go-root>:/usr/local/go:ro \
    -v /tmp/winout:/out -e ROOT=/out/cppgoqt-win -e TMPDIR=/out \
    -w /src cppgo-win64-cross bash windows.sh
```

What that run produced and proved:

- IR extracted from the **Windows** Qt 6.10 headers (host clang, `-target x86_64-w64-mingw32`
  — 696 records, LLP64 widths resolved by clang itself).
- LLP64/purego generation; `shim_qt.dll` (38 MB) compiled and linked by mingw g++
  (`-static-libgcc -static-libstdc++`, so it needs only the Qt DLLs beside it).
- The full runtime smoke, built `CGO_ENABLED=0 GOOS=windows` and executed **under wine**
  with the Qt DLLs on `WINEPATH`: QCoreApplication, signal connect → fire → disconnect,
  a mask-gated virtual override crossing through Windows' `NewCallback` (with the
  `<X>Default` base-call), UTF-8 string round-trips, 2000 GC-finalized value objects, and
  idempotent chain-release `Delete` — all pass.

Two Windows-specific ABI facts the shakedown surfaced, both fixed in the generator (and
harmless on unix, so there is ONE dispatch ABI for all platforms):

- Windows' `syscall.NewCallback` (which purego delegates to there) requires **exactly one
  uintptr-sized result** — every C→Go dispatch function now returns `uintptr` (narrower
  results ride the low bits; the C++ caller's function-pointer type reads only those).
- It also rejects **float arguments** — float callback args now cross as raw IEEE bits in an
  integer register (`__builtin_bit_cast` on the C++ side, `math.Float*frombits` in the Go
  wrapper), mirroring the float-return convention.

wine note: do NOT point `XDG_RUNTIME_DIR` at a makeshift directory to silence wine's warning —
it makes this wine build abort (`free(): invalid pointer`). The warning is harmless.

## Remaining

1. **A confirmation pass on real Windows hardware** — everything above ran under wine, which
   is faithful for this workload but is not Windows. `windows.sh` also supports a native MSYS2
   CLANG64 run (`pacman -S mingw-w64-clang-x86_64-{clang,qt6-base,pkgconf,go}`).
2. **KF6 on Windows** is optional/best-effort (MSYS2 packages exist for parts of KF6); the qt
   base package is the deliverable. webengine/pdf follow the qt recipe with their aqt modules.
