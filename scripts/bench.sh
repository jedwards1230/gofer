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
# It exists for CONCURRENT benchmarks. At -benchtime 1x a fan-out benchmark runs
# one iteration, so how much buffer its peer goroutines happen to allocate is
# decided by scheduling: BenchmarkBroadcastRawEvent's B/op was measured swinging
# 3,920 -> 12,120 (+209%) between identical runs of identical code. No threshold
# both catches a real regression there and stays quiet, which is the
# crying-wolf failure this script exists to avoid. Its allocation COUNT is stable,
# and is what that benchmark's own doc calls its evidence — so the count is what
# gates it.
#
# This is a per-benchmark, stated exemption visible in the baseline, not a blanket
# weakening: every non-concurrent benchmark still gates on both metrics.
#
# The baseline (bench/baseline.txt) is COMMITTED. Updating it is a deliberate
# act that shows up in review as a diff, so a regression cannot be absorbed
# silently — if a change legitimately costs more allocations, the baseline bump
# is where that gets argued.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

baseline="bench/baseline.txt"
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
echo "running benchmarks (-benchtime 1x) over ${pkgs[*]}..." >&2
raw="$(go test -run '^$' -bench . -benchmem -benchtime 1x "${pkgs[@]}" 2>&1)"
echo "$raw"

# Normalize to "name allocs bytes". Benchmark lines look like:
#   BenchmarkX/sub-12   1   123 ns/op   456 B/op   7 allocs/op
# The -N CPU suffix is stripped so a baseline recorded on one core count still
# matches a run on another.
normalize() {
	awk '
		/^Benchmark/ && /allocs\/op/ {
			name = $1
			sub(/-[0-9]+$/, "", name)
			bytes = ""; allocs = ""
			for (i = 1; i <= NF; i++) {
				if ($i == "B/op") bytes = $(i-1)
				if ($i == "allocs/op") allocs = $(i-1)
			}
			if (bytes != "" && allocs != "") print name, allocs, bytes
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
	merged="$current"
	if [ -f "$baseline" ]; then
		merged="$(echo "$current" | while read -r n a b; do
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
	status=0
	while read -r name base_allocs base_bytes base_mode; do
		[ -z "${name:-}" ] && continue
		case "$name" in \#*) continue ;; esac

		line="$(echo "$current" | awk -v n="$name" '$1 == n')"
		if [ -z "$line" ]; then
			echo "MISSING: $name is in the baseline but did not run (renamed or deleted?)" >&2
			status=1
			continue
		fi

		cur_allocs="$(echo "$line" | awk '{print $2}')"
		cur_bytes="$(echo "$line" | awk '{print $3}')"

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
