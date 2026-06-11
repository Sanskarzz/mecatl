package app

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stacklok/mecatl/engine/port"
)

// writeTestFile seeds a real temp soul file (the soul.Store reads through its own
// bounded os.Open seam, so the body must exist on disk; only the BASELINE sidecar is
// faked in memory).
func writeTestFile(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o600)
}

// fakeBaselineIO is an in-memory baselineIO seam so soulguard tests run fully
// offline against no real ~/.config: writes land in a map, reads come from it, and a
// missing key returns fs.ErrNotExist (the TOFU signal). It records write counts so a
// test can prove TOFU writes exactly once.
type fakeBaselineIO struct {
	files  map[string][]byte
	writes int
	// readErr, when set, makes readFile fail with a NON-NotExist error (to exercise
	// the unreadable-baseline fail-soft branch).
	readErr error
	// writeErr, when set, makes writeFile fail (to exercise the establishBaseline
	// write-failure fail-soft branch). The attempt still increments writes.
	writeErr error
}

func newFakeIO() *fakeBaselineIO { return &fakeBaselineIO{files: map[string][]byte{}} }

func (f *fakeBaselineIO) io() baselineIO {
	return baselineIO{
		readFile: func(path string) ([]byte, error) {
			if f.readErr != nil {
				return nil, f.readErr
			}
			data, ok := f.files[path]
			if !ok {
				return nil, fs.ErrNotExist
			}
			return data, nil
		},
		writeFile: func(path string, data []byte) error {
			f.writes++
			if f.writeErr != nil {
				return f.writeErr
			}
			f.files[path] = append([]byte(nil), data...)
			return nil
		},
	}
}

const (
	testSoulPath = "/home/u/.config/mecatl/soul.md"
	testSidecar  = testSoulPath + sidecarSuffix
	hashA        = "aaaa1111"
	hashB        = "bbbb2222"
)

// TestCheckSoulDriftTOFU (R1.2) proves trust-on-first-use: with no sidecar, the
// current hash is written as the baseline exactly once and no drift is reported.
func TestCheckSoulDriftTOFU(t *testing.T) {
	f := newFakeIO()
	if drifted := checkSoulDrift(port.NopDiagnostics{}, f.io(), testSoulPath, hashA, false); drifted {
		t.Fatal("first sighting must not be drift (TOFU)")
	}
	if f.writes != 1 {
		t.Fatalf("TOFU must write the baseline once, got %d writes", f.writes)
	}
	if got := trimHash(string(f.files[testSidecar])); got != hashA {
		t.Fatalf("baseline = %q, want %q", got, hashA)
	}

	// A second load with the SAME hash must NOT rewrite (matching, no drift).
	if drifted := checkSoulDrift(port.NopDiagnostics{}, f.io(), testSoulPath, hashA, false); drifted {
		t.Fatal("matching hash must not be drift")
	}
	if f.writes != 1 {
		t.Fatalf("matching hash must not rewrite the baseline, got %d writes", f.writes)
	}
}

// TestCheckSoulDriftMatching (R1.4) proves a matching baseline reports no drift.
func TestCheckSoulDriftMatching(t *testing.T) {
	f := newFakeIO()
	f.files[testSidecar] = []byte(hashA + "\n") // editor-style trailing newline
	if drifted := checkSoulDrift(port.NopDiagnostics{}, f.io(), testSoulPath, hashA, false); drifted {
		t.Fatal("matching hash must not be drift")
	}
	if f.writes != 0 {
		t.Fatalf("matching hash must not write, got %d writes", f.writes)
	}
}

// TestCheckSoulDriftDiffering (R1.3) proves a differing baseline reports drift and
// does NOT silently rewrite the baseline (the operator must --approve-soul).
func TestCheckSoulDriftDiffering(t *testing.T) {
	f := newFakeIO()
	f.files[testSidecar] = []byte(hashA)
	if drifted := checkSoulDrift(port.NopDiagnostics{}, f.io(), testSoulPath, hashB, false); !drifted {
		t.Fatal("differing hash must report drift")
	}
	if f.writes != 0 {
		t.Fatalf("a drifted soul must NOT rewrite the baseline without --approve-soul, got %d writes", f.writes)
	}
	if got := trimHash(string(f.files[testSidecar])); got != hashA {
		t.Fatalf("baseline must be unchanged on drift, got %q", got)
	}
}

// TestCheckSoulDriftApproveRewrites proves --approve-soul (re)writes the baseline to
// the current hash and clears drift, across all three starting states: a DIFFERING
// prior sidecar (the original differ-then-approve case), NO prior sidecar
// (TOFU+approve), and a MATCHING prior sidecar (idempotent re-approve). In every case
// approve must write the current hash exactly once and report no drift.
func TestCheckSoulDriftApproveRewrites(t *testing.T) {
	t.Run("differing prior baseline", func(t *testing.T) {
		f := newFakeIO()
		f.files[testSidecar] = []byte(hashA)
		if drifted := checkSoulDrift(port.NopDiagnostics{}, f.io(), testSoulPath, hashB, true); drifted {
			t.Fatal("--approve-soul must clear drift")
		}
		if f.writes != 1 {
			t.Fatalf("--approve-soul must rewrite the baseline once, got %d writes", f.writes)
		}
		if got := trimHash(string(f.files[testSidecar])); got != hashB {
			t.Fatalf("--approve-soul must set the baseline to the current hash, got %q want %q", got, hashB)
		}
	})

	t.Run("no prior baseline (TOFU+approve)", func(t *testing.T) {
		f := newFakeIO() // no sidecar at all
		if drifted := checkSoulDrift(port.NopDiagnostics{}, f.io(), testSoulPath, hashA, true); drifted {
			t.Fatal("--approve-soul with no prior baseline must not report drift")
		}
		if f.writes != 1 {
			t.Fatalf("--approve-soul with no prior baseline must write once, got %d writes", f.writes)
		}
		if got := trimHash(string(f.files[testSidecar])); got != hashA {
			t.Fatalf("baseline = %q, want %q (current hash)", got, hashA)
		}
	})

	t.Run("matching prior baseline (idempotent re-approve)", func(t *testing.T) {
		f := newFakeIO()
		f.files[testSidecar] = []byte(hashA + "\n")
		if drifted := checkSoulDrift(port.NopDiagnostics{}, f.io(), testSoulPath, hashA, true); drifted {
			t.Fatal("re-approving a matching baseline must not report drift")
		}
		// Approve is unconditional, so it still writes — the key is that the baseline
		// stays correct (the current hash) afterward.
		if f.writes != 1 {
			t.Fatalf("--approve-soul writes unconditionally, got %d writes", f.writes)
		}
		if got := trimHash(string(f.files[testSidecar])); got != hashA {
			t.Fatalf("re-approve must keep the baseline at the current hash, got %q want %q", got, hashA)
		}
	})
}

// TestCheckSoulDriftWriteFailureFailsSoft (FIX 1) proves the establishBaseline
// write-failure branch: on the TOFU path, if the sidecar write FAILS, checkSoulDrift
// must NOT panic, must report drifted=false, and must degrade soft (the Warn-and-
// return branch in establishBaseline). The write was attempted (writes incremented)
// but no bytes landed.
func TestCheckSoulDriftWriteFailureFailsSoft(t *testing.T) {
	f := newFakeIO() // no sidecar → TOFU path
	f.writeErr = errors.New("disk full")
	if drifted := checkSoulDrift(port.NopDiagnostics{}, f.io(), testSoulPath, hashA, false); drifted {
		t.Fatal("a baseline write failure must not report drift (fail-soft)")
	}
	if f.writes != 1 {
		t.Fatalf("TOFU must ATTEMPT the write once even when it fails, got %d", f.writes)
	}
	if _, ok := f.files[testSidecar]; ok {
		t.Fatalf("a failed write must not persist the sidecar, got %q", f.files[testSidecar])
	}
}

// TestCheckSoulDriftAbsentSoulWritesNothing (R1.5) proves an absent/rejected soul
// (empty current hash) creates no sidecar and reports no drift.
func TestCheckSoulDriftAbsentSoulWritesNothing(t *testing.T) {
	f := newFakeIO()
	if drifted := checkSoulDrift(port.NopDiagnostics{}, f.io(), testSoulPath, "", false); drifted {
		t.Fatal("absent soul must not be drift")
	}
	if f.writes != 0 {
		t.Fatalf("absent soul must write no sidecar, got %d writes", f.writes)
	}
	if len(f.files) != 0 {
		t.Fatalf("absent soul must leave the sidecar map empty, got %v", f.files)
	}
}

// TestCheckSoulDriftUnreadableBaselineFailsSoft proves an unreadable sidecar (a
// non-NotExist error) is treated as "no reliable baseline": no drift, no spurious
// rewrite.
func TestCheckSoulDriftUnreadableBaselineFailsSoft(t *testing.T) {
	f := newFakeIO()
	f.readErr = fs.ErrPermission
	if drifted := checkSoulDrift(port.NopDiagnostics{}, f.io(), testSoulPath, hashA, false); drifted {
		t.Fatal("an unreadable baseline must not report drift (fail-soft)")
	}
	if f.writes != 0 {
		t.Fatalf("an unreadable baseline must not rewrite, got %d writes", f.writes)
	}
}

// TestSidecarPath proves the sidecar is the soul path + ".sha256" (a SIBLING of the
// soul file, in the same directory), and "" for no soul.
func TestSidecarPath(t *testing.T) {
	if got := sidecarPath(testSoulPath); got != testSidecar {
		t.Errorf("sidecarPath = %q, want %q", got, testSidecar)
	}
	if got := sidecarPath(""); got != "" {
		t.Errorf("sidecarPath(\"\") = %q, want \"\"", got)
	}
	// A directory-containing explicit --soul-file path: the sidecar must be a sibling
	// in the SAME directory (locks the "sibling of the soul path" contract — the
	// baseline never escapes the soul's directory).
	const dirSoul = "/tmp/x/soul.md"
	if got := sidecarPath(dirSoul); got != "/tmp/x/soul.md.sha256" {
		t.Errorf("sidecarPath(%q) = %q, want %q", dirSoul, got, "/tmp/x/soul.md.sha256")
	}
	if filepath.Dir(sidecarPath(dirSoul)) != filepath.Dir(dirSoul) {
		t.Errorf("sidecar must be a sibling of the soul file (same dir), got %q for %q",
			sidecarPath(dirSoul), dirSoul)
	}
}

// TestOSWriteSidecarRefusesSymlink (FIX 4, CWE-59) proves the REAL sidecar writer
// refuses to follow a pre-planted symlink at the sidecar path: opening with
// O_NOFOLLOW fails (ELOOP) rather than writing through the link to its target. This
// exercises osWriteSidecar directly (the injectable fake stays a plain map).
func TestOSWriteSidecarRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(target, []byte("original"), 0o600); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	sidecar := filepath.Join(dir, "soul.md.sha256")
	if err := os.Symlink(target, sidecar); err != nil {
		t.Skipf("symlink unsupported on this platform: %v", err)
	}

	err := osWriteSidecar(sidecar, []byte("attacker-controlled-hash\n"))
	if err == nil {
		t.Fatal("osWriteSidecar must REFUSE to write through a symlink (O_NOFOLLOW)")
	}
	// The symlink target must be untouched (the write never reached it).
	got, rerr := os.ReadFile(target)
	if rerr != nil {
		t.Fatalf("read target: %v", rerr)
	}
	if string(got) != "original" {
		t.Fatalf("symlink target was modified through the sidecar write: %q", got)
	}
}

// TestBuildSoulSourceStrictDropsDriftedSoul (R1.6) proves the buildSoulSource-level
// wiring: with an explicit soul file whose hash differs from the recorded baseline
// AND SoulStrict set, buildSoulSourceWith returns nil (no fragment); without
// SoulStrict the same drifted soul still loads (non-nil). It runs offline against the
// fake IO seam — but the soul body is read through the REAL soul.Store, so we point
// SoulPath at a real temp file.
func TestBuildSoulSourceStrictDropsDriftedSoul(t *testing.T) {
	dir := t.TempDir()
	soulPath := dir + "/soul.md"
	if err := writeTestFile(soulPath, "You are terse and kind."); err != nil {
		t.Fatalf("seed soul: %v", err)
	}
	sidecar := soulPath + sidecarSuffix

	// Baseline records a DIFFERENT hash → the current soul is drifted.
	f := newFakeIO()
	f.files[sidecar] = []byte("0000deadbeef")

	// Default (warn-and-load): drifted soul STILL contributes a source.
	if src := buildSoulSourceWith(Config{SoulPath: soulPath}, f.io()); src == nil {
		t.Fatal("default posture must still load a drifted soul (warn-and-load)")
	}

	// --soul-strict: a drifted soul contributes NO fragment (nil source).
	if src := buildSoulSourceWith(Config{SoulPath: soulPath, SoulStrict: true}, f.io()); src != nil {
		t.Fatalf("--soul-strict must drop a drifted soul, got non-nil source %v", src)
	}

	// --no-soul still wins regardless of strict/drift.
	if src := buildSoulSourceWith(Config{SoulPath: soulPath, NoSoul: true, SoulStrict: true}, f.io()); src != nil {
		t.Fatalf("--no-soul must yield nil, got %v", src)
	}
}

// TestBuildSoulSourceStrictMatchingLoads proves --soul-strict does NOT drop a soul
// whose hash MATCHES the baseline (only a drifted one is refused).
func TestBuildSoulSourceStrictMatchingLoads(t *testing.T) {
	dir := t.TempDir()
	soulPath := dir + "/soul.md"
	if err := writeTestFile(soulPath, "You are terse and kind."); err != nil {
		t.Fatalf("seed soul: %v", err)
	}
	// TOFU first to record the matching baseline, then a strict load must keep it.
	f := newFakeIO()
	if src := buildSoulSourceWith(Config{SoulPath: soulPath}, f.io()); src == nil {
		t.Fatal("TOFU load must produce a source")
	}
	if src := buildSoulSourceWith(Config{SoulPath: soulPath, SoulStrict: true}, f.io()); src == nil {
		t.Fatal("--soul-strict must keep a soul whose hash matches the baseline")
	}
}
