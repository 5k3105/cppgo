# cppgo — a semantic C++ → Go binding generator

cppgo generates Go bindings for large C++ libraries. It was built for, and is proven against,
**Qt 6 and KDE Frameworks 6**: it binds whole modules (QtCore + QtGui + QtWidgets as one base
package, plus QtWebEngine, QtPdf, and a KF6/KTextEditor stack) and has driven a real desktop
application in production.

Two things distinguish it:

1. **Semantic type identity.** A clang-based frontend emits a structured, USR-keyed JSON IR —
   every type is a resolved AST node, never a spelled string. The generator plans marshalling
   from that structure. There is no name string-matching anywhere downstream, which eliminates
   the entire bug class that string-based generators patch symbol by symbol.
2. **Two backends from one plan.** The same API surface is emitted either as classic **cgo**
   bindings, or as a fully **cgo-free** backend: `CGO_ENABLED=0` Go that `dlopen`s a prebuilt
   shim library at runtime and calls it through pre-resolved symbol addresses. Your app builds
   like pure Go — fast compiles, cross-tooling friendly — and still drives real Qt.

## Why it was made

[miqt](https://github.com/mappu/miqt) proved that direct C-ABI Qt bindings for Go work well.
But miqt acquires its AST via `clang -ast-dump=json` and reconstructs type identity by
**string-matching spelled names** against global registries. clang has already resolved every
type to a canonical declaration with a unique identity (the USR); discarding that and
re-deriving it from spelling is the root of a recurring bug class:

| miqt fix (historical) | cause | cppgo |
|---|---|---|
| scope-minimal qualification (6 commits, 5 sites) | spelling ≠ registry key | gone — identity is the resolved decl's USR |
| container-inner parsing (`QList<Definition>`) | types are strings | gone — template args are structured nodes |
| typedef chains (`const_pointer→value_type→char`) | manual chain-walking | gone — clang's `getCanonicalType` collapses it |
| array field leak (`_reserved[6]`) | string didn't match a guard | gone — arrays are structured nodes |
| blocked-base member scope | identity rode the inheritance edge | gone — USR is edge-independent |
| `QSpan<>` / `EulerAngles<>` per-type blocks | best-effort emit of unknowns | gone — renderability is a computed decision |
| `int /* TODO */` fallbacks | unmarshallable type guessed at | gone — unbindable methods **drop**, and every drop is counted and reported |

cppgo's exclusions are computed (a policy seed + a closure fixpoint that prunes dead records),
not hand-curated blocklists. The one irreducible exception — classes that compile but have no
linkable symbol in the target library — is a small list *derived by an actual link pass*
(`tools/derive_unlinkable.sh`), because the link is the earliest stage that has that information.

## Feature comparison vs miqt

| Feature | miqt | cppgo |
|---|---|---|
| Full emit (Go + C-ABI header + C++ shim) | yes | yes |
| Multi-package module (base `qt` + dependent packages) | yes | yes |
| Signals (connect/disconnect, typed args) | yes | yes |
| Virtual overrides from Go, with base-call | `super` param injected into the callback | separate `<X>Default` method (plain Go signature) |
| Unregistered virtuals | cross into the binding layer per call | **never leave C++** (per-instance override mask) |
| QObject lifetime (no finalizer; parent/child-managed) | yes | yes |
| Value-type lifetime (GC finalizer + `Delete`) | yes | yes (finalizer-safe: calls pin the wrapper; `Delete` releases the whole embedded chain) |
| Inheritance | flatten (re-emit every inherited method) | Go struct **embedding** (de-flattened — much smaller packages; C++ name-hiding re-declarations still bind correctly) |
| Constructor ordinals | canonical order | same canonical order — `NewClass<N>` numbering matches miqt, so porting is mechanical |
| Free functions | not bound | bound (package-level funcs) |
| Free/friend operators | not bound | bound as methods on the first operand |
| Member operators, conversion operators | member ops partial | bound (`OperatorEqual`, `ToQString`, …) |
| Public data members | get/set synthesized | get/set synthesized |
| Containers | `QList`/`QMap`/… | `QList`/`QVector`/`QSet`/`QMap`/`QHash`/`QPair`/`std::optional` |
| `QString`/`QAnyStringView` ↔ `string` | yes | yes (zero-copy on the way in) |
| cgo-free backend | no | yes (purego/dlopen; direct `SyscallN` calls, no per-call reflection) |
| Coverage transparency | silent skips | per-reason drop summary every run; `CPPGO_DROPLOG` lists every declined symbol |

Where miqt's choices are better, cppgo adopted them (canonical constructor ordering, binding
copy constructors, miqt's enum-constant naming) — the goal is a **drop-in superset**: a miqt
app should port by changing the import path, then gain the extra surface.

## How it works

```
frontend/cppir2.cpp   LibTooling extractor → structured, USR-keyed JSON IR
                      (frontend/cppir.c is a lighter libclang-C-API alternative, same schema)
ir/                   typed IR + JSON load
closure/              which records to bind (policy seed; gen.Fixpoint prunes to a fixpoint)
gen/                  the planner + emitters: per-class Go wrapper, C-ABI header, C++ shim,
                      for the cgo AND purego backends
cmd/gen/              gen <outdir> <pkg> <baseimport|-> <all|Class,...> <ir.json>... 
tools/                parity / link-smoke / symbol-collision checkers
cgofork/              a forked cmd/cgo (BSD-licensed, from the Go project) that skips cgo's
                      per-file gcc discovery probes — a large regen speedup; optional
```

Records cross the ABI as opaque handles with explicit ownership (constructors and by-value
returns are owned and GC-finalized; pointer/reference returns are borrowed — `Delete` on a
borrowed wrapper is designed to be a no-op rather than a use-after-free). Strings cross as
UTF-8 pointer+length. Containers cross as arrays. Virtual dispatch into Go is gated by a
per-instance mask so an un-overridden virtual costs one byte-test in C++.

## Quick start

Prerequisites: Linux x86-64, Go ≥ 1.22, clang/LLVM with dev headers (`llvm`, `clang`,
`libclang-cpp`), Qt 6 dev headers, `pkg-config`. For the KF6 package: KDE Frameworks 6 dev
headers. A **full** 4-package regen wants ~14 GB RAM (see BUILDING.md for the memory knobs);
single modules need far less.

```sh
# Bindability demo: extract IR for a few headers and print the keep/drop report
./run.sh

# Bind one Qt module end-to-end (extract → generate → compile)
./module.sh QtCore

# Full pipeline: base qt (Core+Gui+Widgets) + kf6 + webengine + pdf into one Go module
./all.sh --full        # cold build; plain ./all.sh for the iterative loop

# cgo-free backend: generate with CPPGO_BACKEND=purego, then build the runtime shim lib
CPPGO_BACKEND=purego ./all.sh --full
./buildso.sh /tmp/cppgoqt/qt qt
```

Using the generated bindings (API follows miqt conventions):

```go
import qt "cppgoqt/qt"

app := qt.NewQApplication(os.Args)
w := qt.NewQWidget2()                  // ctor ordinals match miqt
w.SetWindowTitle("hello from Go")
w.OnPaintEvent(func(ev *qt.QPaintEvent) { /* a real C++ virtual override */ })
w.Show()
app.Exec()
```

With the purego backend the same app builds with `CGO_ENABLED=0`; at runtime it loads
`libshim_<pkg>.so` (override the path with `$CPPGO_LIB_<PKG>`). The shim library must be
loaded `RTLD_LOCAL` (the default) — see BUILDING.md.

## Platform support

Linux x86-64 is the primary target. **Windows (cgo-free backend) works, cross-built from
Linux**: `windows.sh` + `docker/win64-cross.Dockerfile` extract IR from the official Windows
Qt headers, generate with the LLP64 data model, compile `shim_qt.dll` with mingw-w64, build
the app `CGO_ENABLED=0 GOOS=windows`, and the full runtime smoke (signals, virtual overrides,
strings, GC lifetime) passes under wine. A confirmation pass on real Windows hardware is the
remaining step. Details: [WINDOWS.md](WINDOWS.md).

## Status and roadmap

Production-proven on a real multi-window Qt/KF6 application (editor, web views, PDF). Known
open items:

- Windows/macOS ports (see WINDOWS.md).
- `QString`-keyed map returns; a few exotic shapes (>15-arg callbacks) stay unbound and are
  listed in the generated skip comments + drop summary.
- Signal emission *from* Go for signals carrying `QPrivateSignal`.
- The out-param heuristic (a non-const scalar pointer parameter binds as an extra return,
  Qt convention) would mis-bind a C-style array-buffer API; fine for Qt/KF6, review it before
  binding an unrelated library.

## License

MIT (see LICENSE). `cgofork/cppgocgo` is a modified copy of Go's `cmd/cgo`, © The Go Authors,
BSD-3-Clause — its license is included alongside it.
