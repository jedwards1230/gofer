#!/usr/bin/env bash
# Run gofer's benchmarks and, optionally, gate on allocation regressions.
#
# WHY A GATE ON ALLOCATIONS AND NOT ON TIME
#
# Wall-clock (ns/op) on a shared GitHub runner varies enough between runs that a
# threshold tight enough to catch a real regression also fires on noise. A gate
# that cries wolf gets ignored, which is worse than no gate — so ns/op is
# REPORTED here and never gates.
#
# allocs/op and B/op are different: at -benchtime 1x they are the exact counts
# for one iteration of deterministic code, so they repeat run to run on any
# machine. They are also the metric that actually tracks the failure mode this
# repo keeps hitting — work that scales with session count or transcript length
# (gofer#298, gofer#308) shows up as allocation growth long before anyone
# notices the latency.
#
# Usage:
#   scripts/bench.sh              # run and print results
#   scripts/bench.sh --check      # compare against the baseline, fail on regression
#   scripts/bench.sh --update     # rewrite the baseline from this run
#
# BOTH metrics gate, and both are needed. allocs/op alone is not enough: the
# gofer#308 quadratic-copy bug moved allocs/op by only -15% while B/op moved
# 854x, because the cost was a few enormous copies rather than many small ones.
# B/op alone is not enough either, since a leak of many tiny objects shows up in
# the count first.
#
# A baseline line may carry an optional 4th field, `allocs-only`, which gates
# that benchmark on allocs/op and NOT on B/op.
#
# It exists for CONCURRENT benchmarks. Go charges allocations PROCESS-WIDE, so a
# fan-out benchmark's peer goroutines land in the timed window too, and at
# -benchtime 1x whether any given peer's read lands inside it is decided by
# scheduling: BenchmarkBroadcastRawEvent's B/op was measured swinging
# 3,920 -> 12,120 (+209%) between identical runs of identical code. No threshold
# both catches a real regression there and stays quiet, which is the crying-wolf
# failure this script exists to avoid.
#
# The allocation COUNT is NOT immune to that stray. An earlier version of this
# comment asserted it was, and gofer#334 measured otherwise: the same mechanism
# moves the count, just with smaller amplitude. What is actually true — and what
# makes the count gateable — is that the stray is a small ABSOLUTE number that
# does not grow with the work one iteration does.
#
# So the obligation sits on the BENCHMARK, not on the gate: a concurrent
# benchmark must batch enough work per iteration that the 25% tolerance is a
# large multiple of that stray. Where it does, the count gates normally and
# nothing here is relaxed. Where it does not, the baseline number is a coin flip
# — BroadcastRawEvent's un-batched baseline of 15 allocs/op sat ~1 allocation
# from failing on UNMODIFIED code, and did fail gofer#332, a PR that touched no
# file in the package under test. It now batches 128 broadcasts per iteration.
# docs/TESTING.md carries the sizing rule and how to re-derive it.
#
# This is a per-benchmark, stated exemption visible in the baseline, not a blanket
# weakening: every non-concurrent benchmark still gates on both metrics.
#
# The baseline (bench/baseline.txt) is COMMITTED. Updating it is a deliberate
# act that shows up in review as a diff, so a regression cannot be absorbed
# silently — if a change legitimately costs more allocations, the baseline bump
# is where that gets argued.
#
# TEST SEAM (not an operator-facing feature): scripts/bench_test.go drives this
# script's parse/threshold logic against synthetic fixtures via two env vars,
# so gofer#344's coverage never shells out to `go test -bench` or touches the
# real committed baseline:
#   BENCH_RAW_FILE      - read `raw` (the `go test -bench` output this script
#                          would otherwise produce) from this file instead of
#                          running the benchmark suite.
#   BENCH_BASELINE_FILE - use this path instead of bench/baseline.txt.
# Both default to the real behavior when unset — CI and a local
# `scripts/bench.sh --check` never set either, so this is inert for them.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

baseline="${BENCH_BASELINE_FILE:-bench/baseline.txt}"
# A stray exported BENCH_RAW_FILE/BENCH_BASELINE_FILE left in a developer's
# shell would otherwise silently redirect a normal --check run with no sign
# in the output that it happened — flag it the same way BENCH_PKGS's
# narrowing is flagged below.
[ -n "${BENCH_BASELINE_FILE:-}" ] && echo "bench.sh: BENCH_BASELINE_FILE override active -> $baseline" >&2
# 25%: loose enough to absorb the odd map-growth or slice-doubling boundary that
# shifts a count without changing the algorithm, tight enough that anything
# accidentally quadratic (which is what actually goes wrong here) blows through
# it immediately.
tolerance=25

mode="run"
case "${1:-}" in
	--check) mode="check" ;;
	--update) mode="update" ;;
	"") ;;
	*)
		echo "usage: scripts/bench.sh [--check|--update]" >&2
		exit 2
		;;
esac

# -benchtime 1x: one iteration per benchmark. Allocation counts are exact and
# reproducible at 1x, and it keeps the lane fast enough to run on every PR. The
# ns/op it yields is near-meaningless, which is fine — nothing gates on it.
# BENCH_PKGS narrows the run for local iteration (e.g.
# BENCH_PKGS=./internal/tui/ scripts/bench.sh --check). It defaults to the whole
# module, which is what CI uses — a narrowed --check still fails on any baseline
# entry that did not run, so narrowing cannot be used to sneak a regression past
# the gate.
# Read into an array so BENCH_PKGS can name several packages
# (BENCH_PKGS="./internal/tui/ ./internal/supervisor/") without relying on
# unquoted word-splitting.
read -ra pkgs <<<"${BENCH_PKGS:-./...}"
if [ -n "${BENCH_RAW_FILE:-}" ]; then
	echo "bench.sh: BENCH_RAW_FILE override active -> $BENCH_RAW_FILE" >&2
	raw="$(cat "$BENCH_RAW_FILE")"
else
	echo "running benchmarks (-benchtime 1x) over ${pkgs[*]}..." >&2
	raw="$(go test -run '^$' -bench . -benchmem -benchtime 1x "${pkgs[@]}" 2>&1)"
fi
echo "$raw"

# Normalize to "name allocs bytes ns". Benchmark lines look like:
#   BenchmarkX/sub-12   1   123 ns/op   456 B/op   7 allocs/op
# The -N CPU suffix is stripped so a baseline recorded on one core count still
# matches a run on another.
#
# ns/op is carried for the JOB SUMMARY only and never reaches the baseline or the
# gate — it is far too noisy on a shared runner to compare against a stored
# number, but it is the figure a human actually wants to see when asking "is this
# still fast?", so the summary shows it and labels it indicative.
normalize() {
	awk '
		/^Benchmark/ && /allocs\/op/ {
			name = $1
			sub(/-[0-9]+$/, "", name)
			bytes = ""; allocs = ""; ns = ""
			for (i = 1; i <= NF; i++) {
				if ($i == "ns/op") ns = $(i-1)
				if ($i == "B/op") bytes = $(i-1)
				if ($i == "allocs/op") allocs = $(i-1)
			}
			if (bytes != "" && allocs != "") print name, allocs, bytes, (ns == "" ? "-" : ns)
		}
	' | sort
}

current="$(echo "$raw" | normalize)"

if [ -z "$current" ]; then
	echo "bench.sh: no benchmark results parsed — did the run fail?" >&2
	exit 1
fi

case "$mode" in
run)
	exit 0
	;;
update)
	mkdir -p bench
	# Carry forward any `allocs-only` marks from the existing baseline. Without
	# this, --update silently re-enables a B/op gate that was deliberately
	# disabled, and the next run starts flapping again.
	merged="$(echo "$current" | awk '{print $1, $2, $3}')"
	if [ -f "$baseline" ]; then
		merged="$(echo "$current" | while read -r n a b _ns; do
			mode="$(awk -v n="$n" '$1 == n && NF >= 4 {print $4}' "$baseline")"
			if [ -n "$mode" ]; then echo "$n $a $b $mode"; else echo "$n $a $b"; fi
		done)"
	fi
	{
		echo "# gofer benchmark baseline: <name> <allocs/op> <B/op> [allocs-only]"
		echo "# Regenerate with scripts/bench.sh --update. Gating: scripts/bench.sh --check."
		echo "# ns/op is deliberately absent — see the header of scripts/bench.sh."
		echo "# A 4th field of 'allocs-only' gates that benchmark on allocs/op only"
		echo "# (concurrent benchmarks: their B/op is scheduling-dependent)."
		echo "$merged"
	} >"$baseline"
	echo "baseline written -> $baseline" >&2
	;;
check)
	if [ ! -f "$baseline" ]; then
		echo "bench.sh: no baseline at $baseline; create one with scripts/bench.sh --update" >&2
		exit 1
	fi

	# A benchmark present in the baseline but missing from this run is a FAILURE,
	# not a pass. Otherwise deleting or renaming a benchmark silently retires its
	# gate, and coverage erodes without anyone deciding to erode it.
	#
	# is_uint rejects a malformed baseline/current field before it reaches bash
	# arithmetic. This is not defensive polish: an unquoted non-numeric operand
	# in a `$(( ))` expression is a NESTED VARIABLE REFERENCE, not a parse
	# error, so a malformed field that happens to collide with another shell
	# variable's name in this script's scope (a baseline row hand-edited to
	# read e.g. "BenchmarkFoo tolerance 1000") silently substitutes that
	# variable's value into the comparison and can report a clean gate.
	# Verified against unmodified bench.sh, gofer#344.
	is_uint() {
		case "$1" in
		'' | *[!0-9]*) return 1 ;;
		*) return 0 ;;
		esac
	}

	status=0
	missing=""
	# Counts baseline rows actually gated (comments/blanks excluded). A
	# baseline that resolves to zero such rows must fail the gate rather than
	# report success — see the zero-rows check after this loop, gofer#344.
	gated_rows=0
	while read -r name base_allocs base_bytes base_mode; do
		[ -z "${name:-}" ] && continue
		case "$name" in \#*) continue ;; esac
		gated_rows=$((gated_rows + 1))

		line="$(echo "$current" | awk -v n="$name" '$1 == n')"
		if [ -z "$line" ]; then
			echo "MISSING: $name is in the baseline but did not run (renamed or deleted?)" >&2
			missing="$missing$name"$'\n'
			status=1
			continue
		fi

		cur_allocs="$(echo "$line" | awk '{print $2}')"
		cur_bytes="$(echo "$line" | awk '{print $3}')"

		if ! is_uint "$base_allocs" || ! is_uint "$base_bytes" || ! is_uint "$cur_allocs" || ! is_uint "$cur_bytes"; then
			echo "MALFORMED: $name has a non-numeric field — baseline allocs=\"$base_allocs\" bytes=\"$base_bytes\", current allocs=\"$cur_allocs\" bytes=\"$cur_bytes\"" >&2
			status=1
			continue
		fi

		for metric in allocs bytes; do
			if [ "$metric" = allocs ]; then
				b="$base_allocs" c="$cur_allocs" label="allocs/op"
			else
				# Concurrent benchmarks gate on the count only — see the header.
				[ "${base_mode:-}" = "allocs-only" ] && continue
				b="$base_bytes" c="$cur_bytes" label="B/op"
			fi
			# Guard against a zero baseline: any growth from 0 is a regression,
			# and the percentage would divide by zero.
			if [ "$b" -eq 0 ]; then
				[ "$c" -gt 0 ] && {
					echo "REGRESSION: $name $label 0 -> $c" >&2
					status=1
				}
				continue
			fi
			pct=$(((c - b) * 100 / b))
			if [ "$pct" -gt "$tolerance" ]; then
				echo "REGRESSION: $name $label $b -> $c (+${pct}%, tolerance ${tolerance}%)" >&2
				status=1
			fi
		done
	done <"$baseline"

	# A baseline that resolves to zero gated rows (empty, or only comments and
	# blank lines) must not report success — the loop above simply never runs,
	# leaving $status at its initial 0. The $current empty-guard earlier in
	# this script covers an empty benchmark RUN, not an empty baseline; this is
	# the baseline-side counterpart. Verified against unmodified bench.sh,
	# gofer#344.
	if [ "$gated_rows" -eq 0 ]; then
		echo "bench.sh: no gate-able rows in $baseline (empty, or only comments/blank lines) — the gate cannot pass on nothing to compare against" >&2
		status=1
	fi

	# GitHub Actions job summary. Written on BOTH outcomes: on a failure it is
	# where you see WHICH numbers moved without opening the log, and on a pass it
	# is the per-PR record of what the hot paths currently cost — which is the
	# whole point of measuring, and is invisible if it only ever lives in a log
	# nobody opens on a green run.
	if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
		{
			echo "## Benchmarks"
			echo
			if [ "$status" -eq 0 ]; then
				echo "No allocation regressions beyond ${tolerance}%."
			else
				if [ -n "$missing" ]; then
					echo "**Benchmarks in the baseline that did not run.** Renamed or deleted?"
					echo "A baseline entry that stops running fails the gate rather than passing"
					echo "quietly, so coverage cannot be retired by accident."
					echo
					echo "$missing" | while read -r m; do
						[ -n "$m" ] && echo "- \`$m\`"
					done
					echo
				fi
				echo "**Allocation regressions**, if any, are marked ⚠ in the table below."
				echo
				echo "If the extra work is intended, re-run \`scripts/bench.sh --update\`"
				echo "and commit the baseline diff with a note on why it is worth it."
			fi
			echo
			echo "| Benchmark | allocs/op | Δ | B/op | Δ | ns/op |"
			echo "|---|---:|---:|---:|---:|---:|"
			# shellcheck disable=SC2016
			# The backticks in the printf formats below are MARKDOWN code spans,
			# not command substitution — single quotes are what keeps them
			# literal, which is the intent.
			echo "$current" | while read -r n a b ns; do
				base="$(awk -v n="$n" '$1 == n {print $2, $3, $4}' "$baseline")"
				if [ -z "$base" ]; then
					printf '| `%s` | %s | new | %s | new | %s |\n' "$n" "$a" "$b" "$ns"
					continue
				fi
				ba="$(echo "$base" | awk '{print $1}')"
				bb="$(echo "$base" | awk '{print $2}')"
				mode="$(echo "$base" | awk '{print $3}')"

				da="—"; [ "$ba" -gt 0 ] && da="$(printf '%+d%%' $(((a - ba) * 100 / ba)))"
				if [ "$mode" = "allocs-only" ]; then
					db="not gated"
				elif [ "$bb" -gt 0 ]; then
					db="$(printf '%+d%%' $(((b - bb) * 100 / bb)))"
				else
					db="—"
				fi

				mark=""
				[ "$ba" -gt 0 ] && [ $(((a - ba) * 100 / ba)) -gt "$tolerance" ] && mark=" ⚠"
				if [ "$mode" != "allocs-only" ] && [ "$bb" -gt 0 ] && [ $(((b - bb) * 100 / bb)) -gt "$tolerance" ]; then
					mark=" ⚠"
				fi

				printf '| `%s`%s | %s | %s | %s | %s | %s |\n' "$n" "$mark" "$a" "$da" "$b" "$db" "$ns"
			done
			echo
			echo "Gated on **allocs/op** and **B/op** against \`bench/baseline.txt\`, tolerance ${tolerance}%."
			echo "**ns/op is indicative only** and never gates — wall-clock on a shared runner is"
			echo "too noisy to threshold. Benchmarks marked \`allocs-only\` in the baseline are"
			echo "concurrent, and their B/op is scheduling-dependent (measured swinging +209%"
			echo "between identical runs), so only their allocation count is gated."
		} >>"$GITHUB_STEP_SUMMARY"
	fi

	if [ "$status" -ne 0 ]; then
		echo "" >&2
		echo "Allocation regressions above. If they are intended, re-run" >&2
		echo "scripts/bench.sh --update and commit the baseline diff with a" >&2
		echo "note on why the extra work is worth it." >&2
		exit 1
	fi
	echo "no allocation regressions beyond ${tolerance}%" >&2
	;;
esac
