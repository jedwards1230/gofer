// Package scripts tests scripts/bench.sh's own parse and threshold logic —
// the one thing standing between an allocation regression and main (gofer#344).
//
// It drives bench.sh via exec.Command against synthetic fixtures under
// testdata/, using the BENCH_RAW_FILE/BENCH_BASELINE_FILE test seam
// documented in bench.sh's header. It never shells out to `go test -bench`
// and never touches the real committed bench/baseline.txt — see
// docs/TESTING.md's "Testing the gate itself" section for why this lives in
// go test rather than a shell-test framework.
package scripts

import (
	_ "embed"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// benchScript is scripts/bench.sh, embedded so it is part of THIS package's
// build graph. `go test`'s result cache keys off the package's source files;
// it has no way to know that bench.sh — read only at runtime, via
// exec.Command — is a dependency of TestBenchGate. Without go:embed, editing
// bench.sh and re-running `go test` with a matching -run pattern can replay a
// stale cached PASS from before the edit, which is exactly the "a green test
// is not evidence" trap this package exists to guard against (hit for real
// during this change's own mutation-testing pass — see the PR description).
// Embedding makes bench.sh's bytes part of the compiled test binary, so
// editing it invalidates the cache the same as editing any other .go file.
//
//go:embed bench.sh
var benchScript []byte

// runCheck runs bench.sh --check, from a temp copy of the embedded script
// (see benchScript), against the given synthetic fixtures, and returns its
// combined stdout+stderr and exit code. rawFile/baselineFile are relative to
// this package's testdata/ dir.
//
// bench.sh cds into its own script's parent directory before reading
// BENCH_RAW_FILE/BENCH_BASELINE_FILE ([repo_root] near its top), so these
// must be resolved to absolute paths here — a path relative to this test
// binary's cwd would resolve against the wrong directory after that cd. The
// temp copy's directory need not be the real repo root: every path bench.sh
// touches under the two overrides is already absolute.
func runCheck(t *testing.T, rawFile, baselineFile string) (output string, exitCode int) {
	t.Helper()

	tmpScript := filepath.Join(t.TempDir(), "bench.sh")
	if err := os.WriteFile(tmpScript, benchScript, 0o755); err != nil { //nolint:gosec // test-only script copy, needs +x
		t.Fatalf("writing embedded bench.sh to temp file: %v", err)
	}

	absRaw, err := filepath.Abs(rawFile)
	if err != nil {
		t.Fatalf("resolving raw fixture path %q: %v", rawFile, err)
	}
	absBaseline, err := filepath.Abs(baselineFile)
	if err != nil {
		t.Fatalf("resolving baseline fixture path %q: %v", baselineFile, err)
	}

	cmd := exec.Command("bash", tmpScript, "--check")
	cmd.Env = append(os.Environ(),
		"BENCH_RAW_FILE="+absRaw,
		"BENCH_BASELINE_FILE="+absBaseline,
	)

	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return string(out), exitErr.ExitCode()
	}
	t.Fatalf("running bench.sh --check: %v\noutput:\n%s", err, out)
	return "", -1
}

func TestBenchGate(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		baseline string
		wantExit int
		// wantContains lines must all appear verbatim in the combined output.
		wantContains []string
		// wantNotContains lines must NOT appear anywhere in the combined output.
		wantNotContains []string
	}{
		{
			// Case 1 (issue floor #1): a regression beyond the 25% tolerance
			// must fail the gate and name the offending row.
			name:     "regression_beyond_tolerance",
			raw:      "testdata/regression_beyond_tolerance_raw.txt",
			baseline: "testdata/regression_beyond_tolerance_baseline.txt",
			wantExit: 1,
			wantContains: []string{
				// Both metrics regress +100% here (10->20 allocs, 1000->2000
				// bytes) — assert both lines, not just one, to pin down that
				// allocs/op and B/op still gate independently.
				"REGRESSION: BenchmarkFoo allocs/op 10 -> 20",
				"REGRESSION: BenchmarkFoo B/op 1000 -> 2000",
			},
		},
		{
			// Case 2 (issue floor #2): allocs/op growing 10->12 (+20%) and
			// B/op growing 1000->1100 (+10%) both sit under the 25%
			// tolerance and must pass cleanly.
			name:     "regression_within_tolerance",
			raw:      "testdata/regression_within_tolerance_raw.txt",
			baseline: "testdata/regression_within_tolerance_baseline.txt",
			wantExit: 0,
			wantContains: []string{
				"no allocation regressions beyond 25%",
			},
			wantNotContains: []string{
				"REGRESSION",
			},
		},
		{
			// Case 3 (issue floor #3): a baseline row (BenchmarkBar) with no
			// matching benchmark in this run must fail as MISSING, not pass
			// silently — a deleted or renamed benchmark cannot quietly
			// retire its own gate.
			name:     "baseline_row_missing_from_run",
			raw:      "testdata/missing_benchmark_raw.txt",
			baseline: "testdata/missing_benchmark_baseline.txt",
			wantExit: 1,
			wantContains: []string{
				"MISSING: BenchmarkBar is in the baseline but did not run",
			},
		},
		{
			// Case 4 (issue floor #4): the check-mode loop iterates the
			// BASELINE, not the current results, so a benchmark that ran but
			// has no baseline row (BenchmarkNew) is invisible to --check by
			// construction — it is neither a MISSING nor a REGRESSION, and
			// the gate must still pass on the benchmark that IS baselined.
			name:     "new_benchmark_has_no_baseline_row",
			raw:      "testdata/new_benchmark_no_baseline_raw.txt",
			baseline: "testdata/new_benchmark_no_baseline_baseline.txt",
			wantExit: 0,
			wantContains: []string{
				"no allocation regressions beyond 25%",
			},
			wantNotContains: []string{
				"MISSING: BenchmarkNew",
				"REGRESSION: BenchmarkNew",
			},
		},
		{
			// Case 5a (issue floor #5, half 1): an allocs-only baseline row
			// must NOT gate on B/op — a 5x byte growth (1000->5000) with an
			// unchanged allocation count must pass.
			name:     "allocs_only_ignores_bytes_growth",
			raw:      "testdata/allocs_only_bytes_grow_raw.txt",
			baseline: "testdata/allocs_only_baseline.txt",
			wantExit: 0,
			wantContains: []string{
				"no allocation regressions beyond 25%",
			},
			wantNotContains: []string{
				"REGRESSION",
			},
		},
		{
			// Case 5b (issue floor #5, half 2): the SAME allocs-only row
			// must still gate on allocs/op — a 100% allocation-count growth
			// (10->20) must fail even though B/op is exempt.
			name:     "allocs_only_still_gates_alloc_count",
			raw:      "testdata/allocs_only_allocs_grow_raw.txt",
			baseline: "testdata/allocs_only_baseline.txt",
			wantExit: 1,
			wantContains: []string{
				"REGRESSION: BenchmarkConcurrent allocs/op 10 -> 20",
			},
		},
		{
			// Case 6 (issue floor #6, highest-value case): a baseline that
			// resolves to zero gate-able rows (comments and blank lines
			// only) must NOT report success. Confirmed against unmodified
			// bench.sh before this test existed — see the mutation table in
			// the PR description for the observed unfixed behavior (exit 0,
			// "no allocation regressions beyond 25%").
			name:     "zero_rows_parsed_from_baseline",
			raw:      "testdata/valid_raw.txt",
			baseline: "testdata/zero_rows_baseline.txt",
			wantExit: 1,
			wantContains: []string{
				"no gate-able rows in",
			},
			wantNotContains: []string{
				"no allocation regressions beyond 25%",
			},
		},
		{
			// Case 7a (issue floor #7): a baseline row with a plainly
			// non-numeric field ("abc") must fail loudly and name the row,
			// not silently skip it.
			name:     "malformed_baseline_field_non_numeric",
			raw:      "testdata/valid_raw.txt",
			baseline: "testdata/malformed_field_baseline.txt",
			wantExit: 1,
			wantContains: []string{
				`MALFORMED: BenchmarkFoo has a non-numeric field`,
			},
			wantNotContains: []string{
				"no allocation regressions beyond 25%",
			},
		},
		{
			// Case 7b: the sharper form of case 7 — a malformed field whose
			// text happens to match another shell variable already in
			// bench.sh's scope ("tolerance"). Before the fix this reached
			// bash arithmetic's nested-variable dereference and silently
			// substituted that variable's numeric value, passing the gate.
			// This is the fixture that actually caught the bug: "abc" alone
			// always aborted loudly via `set -u`, but "tolerance" did not.
			name:     "malformed_baseline_field_nested_variable_collision",
			raw:      "testdata/valid_raw.txt",
			baseline: "testdata/malformed_field_nested_var_baseline.txt",
			wantExit: 1,
			wantContains: []string{
				`MALFORMED: BenchmarkFoo has a non-numeric field`,
			},
			wantNotContains: []string{
				"no allocation regressions beyond 25%",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, exit := runCheck(t, tc.raw, tc.baseline)
			if exit != tc.wantExit {
				t.Fatalf("exit code = %d, want %d\noutput:\n%s", exit, tc.wantExit, out)
			}
			for _, want := range tc.wantContains {
				if !strings.Contains(out, want) {
					t.Errorf("output missing %q\noutput:\n%s", want, out)
				}
			}
			for _, notWant := range tc.wantNotContains {
				if strings.Contains(out, notWant) {
					t.Errorf("output unexpectedly contains %q\noutput:\n%s", notWant, out)
				}
			}
		})
	}
}
