# Building cppgo and generating bindings

## Prerequisites

A Linux x86-64 environment with:

- **Go** ≥ 1.22
- **clang/LLVM with dev headers** — the frontend links `-lclang-cpp -lLLVM`
  (Arch: `llvm clang`; Debian/Ubuntu: `clang-N-dev llvm-N-dev libclang-cpp-N-dev`)
- **Qt 6 dev headers** + `pkg-config` (the scripts use `pkg-config --cflags/--libs Qt6Widgets` etc.)
- for the `kf6` package: KDE Frameworks 6 dev headers
- for the `webengine` / `pdf` packages: the matching Qt modules
- optional but strongly recommended: `ccache`

**Memory:** a cold full 4-package regen peaks around 12–13 GB (the serial Go compile of the huge
generated `qt` package is the long pole). Single-module builds need far less. The scripts already
apply the mitigations (`-gcflags=-N -l`, unity C++ TUs, sequential package builds, `GOTMPDIR` off
tmpfs); on low-RAM machines run under a hard cap (e.g. `systemd-run --user -p MemoryMax=13G`).

## The pipeline (4 stages)

```
frontend/cppir2.cpp  --clang++-->  ./cppir2             the LibTooling frontend binary
./cppir2 <umbrella.h> <filters> <cflags>  > module.json USR-keyed typed IR (JSON)
./cppgogen <outdir> <pkg> <baseimport|-> all module.json emit per-class Go / C-ABI .h / C++ shim
go build                                                 compile the generated package
```

- **Stage 1 — frontend.** `clang++ -std=c++17 -fno-rtti frontend/cppir2.cpp -o ./cppir2 -lclang-cpp -lLLVM`.
- **Stage 2 — extract.** The filter list keeps only declarations whose defining header path contains
  a filter token (`"QtCore,QtGui,QtWidgets"`). The frontend auto-passes clang's `-resource-dir`
  (without it, `size_t` fails to resolve and clang silently degrades types to `int`).
- **Stage 3 — generate.** `cmd/gen` emits one `gen_<class>.{go,h}` per class, C++ shim bodies
  (grouped into unity TUs, see `CPPGO_UNITS`), per-owner enum files, and the support files.
  It prints a per-reason **drop summary** — everything it declined to bind — on every run.
- **Stage 4 — build.** cgo backend: `go build` compiles/links the shims. purego backend: plain
  `CGO_ENABLED=0 go build`, with the shims prebuilt into a `.so` by `buildso.sh`.

## Drivers

| script | what |
|---|---|
| `./run.sh` | bindability demo: extract a few headers, print the KEEP/DROP report |
| `./module.sh QtCore` | one Qt module end-to-end into `/tmp/<Mod>_out` |
| `./mp.sh kf6\|webengine\|pdf` | base `qt` package + one dependent package |
| `./all.sh [--full]` | the whole 4-package `cppgoqt` module into `/tmp/cppgoqt` |
| `./regen_all.sh` | `all.sh` with FRESH IR extraction (needed after a frontend change) |
| `./qtonly.sh` | fast qt-only regen loop for generator iteration (needs cached IR in /tmp) |
| `./buildso.sh <pkgdir> <pkg> [pkg-config pkgs]` | build `libshim_<pkg>.so` for the purego backend (`kf6` is special-cased — no pkg-config) |
| `./windows.sh` | the Windows (MSYS2) purego pipeline — see WINDOWS.md |

`all.sh --full` uses unity C++ TUs (`CPPGO_UNITS=24`, fastest cold build); bare `all.sh` uses
per-class TUs (`CPPGO_UNITS=0`, finest ccache granularity for iteration). Switch modes only with a
clean output dir.

## Environment knobs

| var | meaning |
|---|---|
| `CPPGO_BACKEND` | `""`/`cgo` (default) or `purego` — selects the emitted Go wrapper style |
| `CPPGO_DATAMODEL` | `lp64` (default) or `llp64` (Windows targets) — width of spelled `long`/`wchar_t` |
| `CPPGO_UNITS` | number of aggregate C++ TUs per package (0 = per-class) |
| `CPPGO_DROPLOG` | file path: log every declined binding with its reason + type signature |
| `CPPGO_LIB_<PKG>` | purego runtime: path to `libshim_<pkg>.so` (default: bare soname via the loader) |
| `CPPGO_W` | shim-compile parallelism for the toolexec driver / buildso (default 8/4) |
| `GOTMPDIR` | build temp (defaults to `$HOME/.cache/gobuild-tmp` — keep it off a small tmpfs) |

## The v2 fast-regen pipeline (cgo backend)

`all.sh` wires three optimizations ("legs") together — a cold 4-package regen runs in ~3 minutes
instead of ~an hour:

1. **Forked cgo** (`cgofork/cppgocgo`, BSD — a modified copy of Go's `cmd/cgo`): skips cgo's three
   per-file gcc discovery probes, which dominate cgo time on thousands of generated files.
   `cgofork/restore_cppgocgo.sh` syncs it into `$GOROOT/src/cmd/cppgocgo` (it must live there to
   use `cmd/internal/*`); `all.sh` builds it and substitutes it via `go build -toolexec`.
2. **Per-shape trampoline collapse**: shim calls are routed through shape-generic dispatchers
   (`cppgo_shims.h`), so cgo emits one trampoline per distinct C signature (~1.2k) instead of per
   call (~32k). A compile-time oracle (`cppgo_verify.c`, enabled with `-DCPPGO_VERIFY`) proves
   every dispatcher cast matches the real shim prototype.
3. **Out-of-package shims**: the C++ shim bodies live in `cshim/` and are compiled by the toolexec
   wrapper (`cgofork/cppgo_toolexec.sh`) with the clang driver against a full-header PCH, into
   `cshim/libshim.a`, linked via `#cgo LDFLAGS`. `go build` never compiles C++ itself.

None of this is needed for the purego backend, which is a plain Go build.

## The purego (cgo-free) backend

```sh
CPPGO_BACKEND=purego ./all.sh --full        # emit cgo-free Go + the shim sources
./buildso.sh /tmp/cppgoqt/qt qt             # compile shims -> libshim_qt.so
CGO_ENABLED=0 go build ./yourapp            # pure-Go build; dlopens the .so at runtime
```

Per-call dispatch is `purego.SyscallN` on symbol addresses resolved once at init — no per-call
reflection. Signatures that pass or return a float by value fall back to purego's
`RegisterLibFunc` (SyscallN is integer-register only).

**⚠️ RTLD_LOCAL is load-bearing.** Every `libshim_<pkg>.so` carries its own copy of the C→Go
dispatch pointers. If the libraries were loaded `RTLD_GLOBAL`, those duplicate symbols would
interpose across packages and every signal/virtual would silently no-op. The generated loader
uses `RTLD_LAZY` without `RTLD_GLOBAL` — keep it that way. Diagnostic if it regresses:
`nm -D libshim_qt.so | grep cppgo_virt` on two package libs → duplicate definitions +
`RTLD_GLOBAL` = dead callbacks.

## Verifying a regen

- `go build` exit code + error count yourself — don't trust a driver script's filtered tail.
  Capture `$?` immediately; C++ errors match `error:`, Go errors match `\.go:\d+:\d+:`.
- `tools/check_link_smoke.sh` — links a tiny executable per package + combinations; catches
  undefined-symbol classes a package archive build cannot (a `.a` compiles fine with undefined
  externals; only an executable link exposes them).
- `tools/check_symbol_collisions.py` — static cross-package duplicate-symbol scan.
- `tools/check_method_parity.py` / `check_ctor_parity.py` / `check_default_parity.py` — diff the
  generated tree against a miqt checkout (`MIQT_QT_DIR`) to confirm superset coverage and
  matching constructor ordinals.
- The generator's drop summary (stderr) + `CPPGO_DROPLOG` for the full declined list.

## App binary size

cppgo binds the whole Qt surface; an app links a fraction. The effective recipe is applied at the
app build: `go build -ldflags "-s -w"` (+ optional `upx`). For the cgo backend you can additionally
enable cross-language DCE per app: `-ffunction-sections -fdata-sections` on the shim compile plus
`-Wl,--gc-sections` at link (Go's cgo flag allowlist requires `CGO_CXXFLAGS_ALLOW` for these).
