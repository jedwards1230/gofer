package tui

// filemention_cost_test.go covers the second class of regression the benchmark
// gate cannot see: CPU-bound compare and sort work (gofer#315's meta-finding).
//
// scripts/bench.sh gates on allocs/op and B/op. [matchFilePaths] is the clearest
// instance in the codebase of a hot path those two metrics are blind to: it
// lowercases and scans up to config.DefaultFileMentionMaxEntries (5,000) paths
// and then sorts every match, before the 50-row popup limit is applied — real
// per-keystroke work, in FIVE allocations. Writing a benchmark for it would not
// help: the gate would look at 5 allocs/op and see nothing to gate on, no
// matter how slow the comparison got.
//
// So the coverage here is a SCALING check and an ORDER check, not a benchmark.
// Between them they pin the two ways this can go wrong: it grows super-linearly
// in the candidate count, or it gets faster by ranking worse.
//
// WHY A RATIO AND NOT A MILLISECOND BUDGET. The first version of this file
// asserted an absolute ceiling — 20ms per call against the ~0.5ms it actually
// takes. That bound turned out to be unfalsifiable by anything short of a
// catastrophe: a mutation replacing the ranking sort with a bubble sort over
// every match — a genuinely quadratic algorithm — sailed under it, so the
// assertion was green against precisely the regression it was written to catch.
// Tightening the number is not the fix either; the headroom is there because
// wall-clock on a shared runner is noisy, which is the same reason
// scripts/bench.sh refuses to gate on ns/op at all.
//
// Comparing the function against ITSELF at two candidate counts takes the
// machine out of the assertion. 4x the candidates costs about 4x the time when
// the work is linear and about 16x when it is quadratic, on a fast laptop and a
// loaded runner alike, so the threshold can sit between those two without
// depending on how fast either machine is.

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jedwards1230/gofer/internal/config"
)

// mentionCandidates builds a sorted candidate list shaped like a real
// repository — nested directories, several path families — and, importantly,
// one in which [matchQuery] matches on more than one ranking tier, with the
// WEAKER matches sorting earlier alphabetically.
//
// That last property is load-bearing, and it was missing from this fixture's
// first version: when every match lands in a single tier, "the returned rows
// are all tier 0" holds no matter when the limit is applied, and the ranking
// assertion below passed against a truncate-before-rank mutation.
//
// With "zap" as the query:
//
//	zap/core_NNNN.go           whole-path prefix   -> rank 0, sorts LAST
//	internal/zapped_NNNN.go    base-name prefix    -> rank 1
//	internal/unzap_NNNN.go     substring only      -> rank 2, sorts first
//
// so a limit applied before the sort returns rank-2 rows and nothing else.
func mentionCandidates(n int) []string {
	out := make([]string, 0, n)
	for i := range n {
		switch i % 4 {
		case 0:
			out = append(out, fmt.Sprintf("zap/core_%04d.go", i))
		case 1:
			out = append(out, fmt.Sprintf("internal/zapped_%04d.go", i))
		case 2:
			out = append(out, fmt.Sprintf("internal/unzap_%04d.go", i))
		default:
			out = append(out, fmt.Sprintf("docs/notes/%04d-design.md", i))
		}
	}
	sort.Strings(out)
	return out
}

// matchQuery is the query every test in this file uses; see [mentionCandidates]
// for why it is this string and not something more obvious.
const matchQuery = "zap"

// timeMatch reports the average cost of one matchFilePaths call over paths.
func timeMatch(paths []string) time.Duration {
	const runs = 50
	start := time.Now()
	for range runs {
		_ = matchFilePaths(paths, matchQuery, menuCandidateLimit)
	}
	return time.Since(start) / runs
}

// timeMatchPair measures both sizes, ALTERNATING and keeping each side's
// fastest round. Two things this buys, both about making the ratio trustworthy
// rather than the absolute numbers precise:
//
// Alternating means a load spike partway through the test lands on both sides
// rather than inflating whichever one happened to run during it — a spike that
// hit only the large measurement would look exactly like super-linear growth.
// Taking the minimum is the right estimator here because noise is one-sided:
// contention, scheduling and frequency scaling can only ever make a run slower,
// so the fastest round of several is the closest to the work itself.
func timeMatchPair(t *testing.T, small, large int) (time.Duration, time.Duration) {
	t.Helper()
	smallPaths := mentionCandidates(small)
	largePaths := mentionCandidates(large)

	for _, p := range [][]string{smallPaths, largePaths} {
		// One untimed call per fixture, so first-touch page faults land
		// outside the measurement — and assert it matched enough rows to fill
		// the popup, since below the limit this measures a much cheaper call
		// than the one it claims to.
		if got := matchFilePaths(p, matchQuery, menuCandidateLimit); len(got) != menuCandidateLimit {
			t.Fatalf("matched %d rows for %q over %d candidates, want the popup limit %d",
				len(got), matchQuery, len(p), menuCandidateLimit)
		}
	}

	smallCost, largeCost := time.Duration(math.MaxInt64), time.Duration(math.MaxInt64)
	for range 3 {
		smallCost = min(smallCost, timeMatch(smallPaths))
		largeCost = min(largeCost, timeMatch(largePaths))
	}
	return smallCost, largeCost
}

// TestMatchFilePathsScalesLinearlyInCandidates is the wall-clock half. The
// function runs on every keystroke while an `@` token is open, over up to
// config.DefaultFileMentionMaxEntries candidates, and the allocation gate
// cannot see it at any speed.
//
// WHY 16x INPUT AND NOT 4x. The first version compared 1,250 against 5,000 —
// a 4x step, where linear predicts 4x and quadratic 16x. That is a separation
// of only ~3.6x between healthy and broken, and CI ate most of it: this
// machine read a healthy 4.0x while linux/amd64 CI read a healthy **6.6x** on
// the same commit, against a 6.0 threshold picked from local runs. The test
// went red on healthy code.
//
// Widening the step fixes that at the source rather than by moving the
// threshold. At 16x, linear predicts 16x and quadratic 256x, and the measured
// endpoints on this machine are:
//
//	healthy    14.5x
//	quadratic 257.5x   (ranking sort replaced with a bubble sort)
//
// — a separation of ~17.8x instead of ~3.6x. maxRatio can then sit far from
// BOTH endpoints instead of splitting a narrow gap, which is what makes it a
// property of the algorithm rather than of the machine. Even applying CI's
// observed 1.65x inflation to the healthy figure lands near 24x, still 4x
// under the threshold.
//
// The production cap (config.DefaultFileMentionMaxEntries, 5,000) sits between
// the two measurement points on purpose, so the curve is sampled across the
// operating range rather than beyond it. The large end deliberately exceeds the
// cap: this measures the SHAPE of the curve, not a scenario an operator hits.
//
// Do not raise maxRatio to quiet a failure. If this fires, either the ranking
// really did become super-linear, or the runner is so contended that a linear
// scan reads 100x for a 16x input — and that is worth knowing either way.
func TestMatchFilePathsScalesLinearlyInCandidates(t *testing.T) {
	const small = config.DefaultFileMentionMaxEntries / 4 // 1,250
	const large = small * 16                              // 20,000

	const maxRatio = 100.0

	smallCost, largeCost := timeMatchPair(t, small, large)
	if smallCost <= 0 {
		t.Fatalf("the %d-candidate call measured %s — too fast to time, which would make the ratio below meaningless", small, smallCost)
	}
	ratio := float64(largeCost) / float64(smallCost)

	// Logged unconditionally, pass or fail. Wall-clock behaviour on whatever
	// runner this lands on is exactly the data nobody has when they come to
	// write the next timing assertion, and a number that only appears on
	// failure is a number nobody ever sees.
	t.Logf("matchFilePaths: %s at %d candidates, %s at %d — %.1fx for 16x the input (linear ~16x, quadratic ~256x)",
		smallCost, small, largeCost, large, ratio)

	if ratio > maxRatio {
		t.Errorf("16x the candidates cost %.1fx the time (%s -> %s), want under %.0fx — linear would be ~16x and quadratic ~256x. matchFilePaths runs on every keystroke while an @ token is open, in FIVE allocations no matter how slow it gets, so scripts/bench.sh cannot see this and this assertion is the only thing watching it",
			ratio, smallCost, largeCost, maxRatio)
	}
}

// TestMatchFilePathsRanksPrefixesFirstAtScale is the correctness half, and it
// is what stops the scaling check above from being satisfiable by making the
// function worse: applying the 50-row limit before the ranking sort is both
// faster and wrong.
//
// TestMatchFilePathsRanksPrefixesFirst (filemention_test.go) already asserts
// the tier order — over a FOUR-path fixture, which is smaller than the popup
// limit, so a truncate-before-rank bug cannot show there. This one runs at the
// 5,000-candidate ceiling, where far more paths match than the limit returns.
func TestMatchFilePathsRanksPrefixesFirstAtScale(t *testing.T) {
	paths := mentionCandidates(config.DefaultFileMentionMaxEntries)

	got := matchFilePaths(paths, matchQuery, menuCandidateLimit)
	if len(got) != menuCandidateLimit {
		t.Fatalf("matched %d rows, want the popup limit %d", len(got), menuCandidateLimit)
	}
	for i, p := range got {
		if !strings.HasPrefix(p, "zap/") {
			t.Fatalf("row %d is %q, which matches %q only as a substring, yet %d whole-path prefix matches exist and every one of them outranks it — the 50-row limit is being applied before the ranking sort",
				i, p, matchQuery, countPrefixed(paths, "zap/"))
		}
	}
}

func countPrefixed(paths []string, prefix string) int {
	n := 0
	for _, p := range paths {
		if strings.HasPrefix(p, prefix) {
			n++
		}
	}
	return n
}
