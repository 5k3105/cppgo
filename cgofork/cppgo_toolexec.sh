#!/bin/sh
# cppgo v2 toolexec wrapper — glues legs 1 + 3 into `go build -toolexec=...`.
#
#   leg 1 (splice):  substitute the forked cgo ($CPPGO_CGO) for the stock cgo tool — it skips the 3
#                    per-file gcc discovery probes (loadDWARF/guessKinds/loadDefines).
#   leg 3 (driver): after cgo emits _cgo_export.h into its -objdir, compile the package's cshim/*.cpp with
#                    the clang DRIVER (clang++ -c, batched -P W, against a full-header PCH built here once)
#                    -> cshim/libshim.a, which the package links via #cgo LDFLAGS. The driver emits ODR-used
#                    inline closures as weak defs, so libshim.a is self-contained for an EXECUTABLE link
#                    (in-process libtooling dropped them; see BUILDING.md). cgo's own _cgo_export.h is fed
#                    with -include so the shims' cppgoOn* callback decls resolve from _cgo_export.c.
#
# Env (set by the build driver, e.g. all.sh):
#   CPPGO_CGO        path to the forked cgo binary (default /tmp/cgofork/cgo)
#   CPPGOSHIM_CXX    clang flags for the shim parse/compile (Qt -I/-cflags, -std, -O0, -fPIC, -resource-dir)
#   CPPGO_W          driver-compile workers (default 8)
set -u

tool=$1
case "$tool" in
*/cgo)
	shift
	# Find -objdir (holds _cgo_export.h) and the package dir (dirname of a .go input).
	objdir=""
	pkgdir=""
	prev=""
	for a in "$@"; do
		[ "$prev" = "-objdir" ] && objdir=$a
		case "$a" in *.go) pkgdir=$(dirname "$a") ;; esac
		prev=$a
	done

	# leg 1: run the forked cgo in place of stock cgo.
	t0=$(date +%s.%N)
	"${CPPGO_CGO:-/tmp/cgofork/cgo}" "$@" || exit $?
	[ -n "${CPPGO_TIME:-}" ] && t1=$(date +%s.%N) && echo "TIMING cgo(leg1) $(echo "$t1-$t0" | bc)s pkg=$pkgdir" >&2

	# leg 3: build libshim.a from the out-of-package shims, if this package has any.
	if [ -n "$pkgdir" ] && [ -d "$pkgdir/cshim" ]; then
		# Full-header PCH so Qt parses ONCE: collect exactly the headers the shims include (minus the
		# per-class gen_*.h and the cgo export hdr) and precompile them. Built/cached per package; if the
		# PCH build fails, pchopt stays empty and the compile re-parses (correct, just slower).
		pchopt=""
		umb="$pkgdir/cshim/cppgo_pch_full.h"
		pch="$pkgdir/cshim/cppgo_pch_full.pch"
		grep -h '^#include' "$pkgdir"/cshim/*.cpp | grep -vE 'gen_|_cgo_export' | sort -u > "$umb"
		if [ ! -f "$pch" ] || [ "$umb" -nt "$pch" ]; then
			clang++ -x c++-header ${CPPGOSHIM_CXX:-} -I"$pkgdir" "$umb" -o "$pch" 2>/dev/null || pch=""
		fi
		[ -n "$pch" ] && [ -f "$pch" ] && pchopt="-include-pch $pch"
		# Compile the shims with the clang DRIVER (clang++ -c) — NOT in-process libtooling. The driver
		# emits ODR-used inline helpers + constexpr template instantiations (std::piecewise_construct,
		# std::forward_as_tuple, std::get<>, Qt inline methods) as weak defs, so cshim/libshim.a is
		# self-contained for an EXECUTABLE link. In-process EmitObjAction leaves them as bare external
		# declarations -> undefined at link; that never surfaced because cppgo only ever built packages,
		# never an exe. $pchopt loads the full-header PCH so Qt is parsed once into the
		# PCH. We BATCH the shims into ~W groups (xargs -n) and run ONE clang++ per group in parallel
		# (-P W): each invocation loads the 124 MB PCH ONCE and compiles its whole group, so the PCH is
		# loaded ~W times, not once per file — restoring the in-process split's amortization (per-file
		# loading regressed the regen ~2.6x). Compile in a scratch dir so each .cpp -> <basename>.o there.
		excg=""; [ -n "$objdir" ] && [ -f "$objdir/_cgo_export.h" ] && excg="-include $objdir/_cgo_export.h"
		t2=$(date +%s.%N)
		scd="$pkgdir/cshim/.objs"; rm -rf "$scd"; mkdir -p "$scd"
		ncpp=$(ls "$pkgdir"/cshim/*.cpp | wc -l); W="${CPPGO_W:-8}"
		batch=$(( (ncpp + W - 1) / W )); [ "$batch" -lt 1 ] && batch=1
		( cd "$scd" && ls "$pkgdir"/cshim/*.cpp | xargs -n "$batch" -P "$W" \
			clang++ $pchopt ${CPPGOSHIM_CXX:-} -I"$pkgdir" $excg -c ) || exit $?
		nobj=$(ls "$scd"/*.o 2>/dev/null | wc -l)
		[ "$nobj" -eq "$ncpp" ] || { echo "shim(driver): $nobj/$ncpp objects — a worker failed" >&2; exit 1; }
		rm -f "$pkgdir/cshim/libshim.a"
		ar rcs "$pkgdir/cshim/libshim.a" "$scd"/*.o || exit $?
		rm -rf "$scd"
		[ -n "${CPPGO_TIME:-}" ] && t3=$(date +%s.%N) && echo "TIMING shim(driver) $(echo "$t3-$t2" | bc)s" >&2
		# Measurement hook: compile the cgo-emitted C glue (stage 3) ourselves and time it, then abort —
		# isolates the .cgo2.c trampolines + _cgo_export.c compile without running gc/link. Unset normally.
		if [ -n "${CPPGO_MEAS_STAGE3:-}" ] && [ -n "$objdir" ]; then
			: > /tmp/s3cc.err
			# (a) the per-file trampolines (678 .cgo2.c), compiled in parallel
			ta=$(date +%s.%N)
			ls "$objdir"/*.cgo2.c 2>/dev/null | xargs -P "${CPPGO_W:-8}" -I{} gcc -O0 -g0 -fPIC -I"$objdir" -I"$pkgdir" -c {} -o {}.o 2>>/tmp/s3cc.err
			tb=$(date +%s.%N)
			# (b) the export glue (one big _cgo_export.c with the 8438 callbacks) + _cgo_main.c
			for c in "$objdir"/_cgo_export.c "$objdir"/_cgo_main.c; do
				[ -f "$c" ] && gcc -O0 -g0 -fPIC -I"$objdir" -I"$pkgdir" -c "$c" -o "$c.o" 2>>/tmp/s3cc.err
			done
			tc=$(date +%s.%N)
			# (c) one .cgo2.c WITHOUT cppgo_shims.h, to isolate the per-file inline-dispatcher re-parse cost
			one=$(ls "$objdir"/*.cgo2.c 2>/dev/null | head -1)
			td=$(date +%s.%N); [ -n "$one" ] && gcc -O0 -g0 -fPIC -I"$objdir" -I"$pkgdir" -c "$one" -o "$one.x.o" 2>>/tmp/s3cc.err; te=$(date +%s.%N)
			grep -v 'cppgo_shims.h' "$one" > "$one.noshim.c" 2>/dev/null
			tf=$(date +%s.%N); gcc -O0 -g0 -fPIC -I"$objdir" -I"$pkgdir" -c "$one.noshim.c" -o "$one.noshim.o" 2>>/tmp/s3cc.err; tg=$(date +%s.%N)
			echo "STAGE3a cgo2.c (x$(ls "$objdir"/*.cgo2.c 2>/dev/null | wc -l), -P${CPPGO_W:-8}) = $(echo "$tb-$ta" | bc)s" >&2
			echo "STAGE3b _cgo_export.c+main = $(echo "$tc-$tb" | bc)s" >&2
			echo "STAGE3c one .cgo2.c: with-shims=$(echo "$te-$td" | bc)s  without-shims=$(echo "$tg-$tf" | bc)s" >&2
			echo "STAGE3 errs=$(grep -c 'error:' /tmp/s3cc.err)" >&2
			exit 1
		fi
		# Validation hook: stop the build after the shim compile (skip gc/link) so stage 1+2 can be
		# measured without a full regen. Unset in normal builds.
		[ -n "${CPPGO_STOP_AFTER_SHIM:-}" ] && exit 1
	fi
	exit 0
	;;
*)
	exec "$@"
	;;
esac
