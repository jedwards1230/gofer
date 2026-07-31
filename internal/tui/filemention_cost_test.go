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
func TestMatchFilePathsScalesLinearlyInCandidates(t *testing.T) {
	const small = config.DefaultFileMentionMaxEntries / 4 // 1,250
	const large = config.DefaultFileMentionMaxEntries     // 5,000

	// 4x the candidates. The threshold is set from BOTH measured endpoints
	// rather than from theory: as written the ratio is 3.7-4.0x, stable across
	// repeated runs, and against a mutation replacing the ranking sort with a
	// quadratic one it is 14.3x. 6.0 sits between them, ~1.5x above the healthy
	// figure and well under the broken one.
	//
	// The first draft of this test asserted an absolute 20ms budget instead;
	// the quadratic mutation passed it at ~10ms. The threshold here is chosen
	// against a measured failure, not against a guess.
	const maxRatio = 6.0

	smallCost, largeCost := timeMatchPair(t, small, large)
	if smallCost <= 0 {
		t.Fatalf("the %d-candidate call measured %s — too fast to time, which would make the ratio below meaningless", small, smallCost)
	}
	ratio := float64(largeCost) / float64(smallCost)
	t.Logf("matchFilePaths: %s at %d candidates, %s at %d — %.1fx for 4x the input", smallCost, small, largeCost, large, ratio)

	if ratio > maxRatio {
		t.Errorf("4x the candidates cost %.1fx the time (%s -> %s), want under %.0fx. matchFilePaths runs on every keystroke while an @ token is open, in FIVE allocations no matter how slow it gets — scripts/bench.sh cannot see this, so this assertion is the only thing watching it",
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
