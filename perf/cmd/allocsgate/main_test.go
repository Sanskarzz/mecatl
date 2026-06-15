package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// benchLine builds one realistic `go test -bench -benchmem` result line for the
// given decorated name and allocs/op (innocuous numbers — never a destructive
// literal). The ns/op and B/op figures are filler the parser must skip past.
func benchLine(name string, allocs int) string {
	return name + "   \t 1000000\t   942.0 ns/op\t   856 B/op\t   " +
		itoa(allocs) + " allocs/op"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// writeBench writes lines (joined by newlines) to a temp file and returns its path.
func writeBench(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "bench.txt")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestGate(t *testing.T) {
	cases := []struct {
		name     string
		old      []string // nil => baseline file is absent (first run)
		new      []string
		wantExit int
		wantSub  string // a substring the summary must contain
	}{
		{
			name:     "regression caught 21->26",
			old:      []string{benchLine("BenchmarkBuild-12", 21)},
			new:      []string{benchLine("BenchmarkBuild-12", 26)},
			wantExit: 1,
			wantSub:  "regression",
		},
		{
			name:     "identical passes",
			old:      []string{benchLine("BenchmarkBuild-12", 21)},
			new:      []string{benchLine("BenchmarkBuild-12", 21)},
			wantExit: 0,
			wantSub:  "no regression",
		},
		{
			// +0.5% (under the 2% relative floor) — passes even though the
			// absolute delta is >=1.
			name:     "199->200 +0.5% passes",
			old:      []string{benchLine("BenchmarkHeuristicCompact-12", 199)},
			new:      []string{benchLine("BenchmarkHeuristicCompact-12", 200)},
			wantExit: 0,
			wantSub:  "no regression",
		},
		{
			// +7.7% AND +1 absolute — both thresholds crossed, fails.
			name:     "13->14 +7.7% fails",
			old:      []string{benchLine("BenchmarkSplitCommands-12", 13)},
			new:      []string{benchLine("BenchmarkSplitCommands-12", 14)},
			wantExit: 1,
			wantSub:  "regression",
		},
		{
			// 0 -> nonzero: infinite ratio, fails.
			name:     "0->5 fails",
			old:      []string{benchLine("BenchmarkZeroAlloc-12", 0)},
			new:      []string{benchLine("BenchmarkZeroAlloc-12", 5)},
			wantExit: 1,
			wantSub:  "regression",
		},
		{
			// +1 absolute but the relative floor is not crossed at a high base.
			name:     "high-base +1 passes (under 2%)",
			old:      []string{benchLine("BenchmarkBig-12", 1000)},
			new:      []string{benchLine("BenchmarkBig-12", 1001)},
			wantExit: 0,
			wantSub:  "no regression",
		},
		{
			name:     "empty new fails closed",
			old:      []string{benchLine("BenchmarkBuild-12", 21)},
			new:      []string{"goos: linux", "PASS"}, // no benchmark lines
			wantExit: 1,
			wantSub:  "ZERO benchmarks",
		},
		{
			name:     "missing old skips green",
			old:      nil, // file absent
			new:      []string{benchLine("BenchmarkBuild-12", 21)},
			wantExit: 0,
			wantSub:  "first run",
		},
		{
			name:     "corrupt old (present but empty) fails loud",
			old:      []string{"garbage", "not a bench line"},
			new:      []string{benchLine("BenchmarkBuild-12", 21)},
			wantExit: 1,
			wantSub:  "corrupt",
		},
		{
			name:     "no overlap warns not fails",
			old:      []string{benchLine("BenchmarkOldName-12", 21)},
			new:      []string{benchLine("BenchmarkNewName-12", 21)},
			wantExit: 0,
			wantSub:  "no benchmark name overlaps",
		},
		{
			// Sub-benchmark + -N suffix in the decorated key, parsed and matched.
			name:     "sub-benchmark + suffix keys parse and regress",
			old:      []string{benchLine("BenchmarkEvaluatorEvaluate/simple-tool-12", 13)},
			new:      []string{benchLine("BenchmarkEvaluatorEvaluate/simple-tool-12", 16)},
			wantExit: 1,
			wantSub:  "regression",
		},
		{
			name:     "sub-benchmark identical passes",
			old:      []string{benchLine("BenchmarkEvaluatorEvaluate/compound-bash-12", 29)},
			new:      []string{benchLine("BenchmarkEvaluatorEvaluate/compound-bash-12", 29)},
			wantExit: 0,
			wantSub:  "no regression",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			newPath := writeBench(t, tc.new...)
			var oldPath string
			if tc.old == nil {
				oldPath = filepath.Join(t.TempDir(), "absent.txt") // does not exist
			} else {
				oldPath = writeBench(t, tc.old...)
			}

			r := gate(oldPath, newPath)
			if r.exitCode != tc.wantExit {
				t.Errorf("exit = %d, want %d\nsummary:\n%s", r.exitCode, tc.wantExit, r.summary)
			}
			if tc.wantSub != "" && !strings.Contains(r.summary, tc.wantSub) {
				t.Errorf("summary missing %q\ngot:\n%s", tc.wantSub, r.summary)
			}
		})
	}
}

// TestGateMedianAcrossSamples confirms -count=N samples collapse to the median, so
// the gate decision is robust to one stray line.
func TestGateMedianAcrossSamples(t *testing.T) {
	old := writeBench(t,
		benchLine("BenchmarkBuild-12", 21),
		benchLine("BenchmarkBuild-12", 21),
		benchLine("BenchmarkBuild-12", 21),
	)
	// Three new samples: 21, 21, 99 — median is 21, NOT a regression despite the
	// outlier.
	cur := writeBench(t,
		benchLine("BenchmarkBuild-12", 21),
		benchLine("BenchmarkBuild-12", 21),
		benchLine("BenchmarkBuild-12", 99),
	)
	if r := gate(old, cur); r.exitCode != 0 {
		t.Errorf("median should ignore the outlier; exit = %d\n%s", r.exitCode, r.summary)
	}
}

func TestParseAllocs(t *testing.T) {
	out := strings.Join([]string{
		"goos: linux",
		"goarch: amd64",
		benchLine("BenchmarkBuild-12", 21),
		benchLine("BenchmarkEvaluatorEvaluate/plain-bash-12", 18),
		"PASS",
	}, "\n")
	got := parseAllocs(out)
	if got["BenchmarkBuild-12"] != 21 {
		t.Errorf("BenchmarkBuild-12 = %v, want 21", got["BenchmarkBuild-12"])
	}
	if got["BenchmarkEvaluatorEvaluate/plain-bash-12"] != 18 {
		t.Errorf("sub-benchmark = %v, want 18", got["BenchmarkEvaluatorEvaluate/plain-bash-12"])
	}
	if len(got) != 2 {
		t.Errorf("parsed %d benchmarks, want 2: %v", len(got), got)
	}
}
