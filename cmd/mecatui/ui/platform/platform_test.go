package platform

import (
	"runtime"
	"strings"
	"testing"
)

func TestCurrentDetectsRuntime(t *testing.T) {
	// An empty value is neither "mac" nor "pc", so Current() falls through to
	// the runtime.GOOS check. t.Setenv auto-restores the prior value on exit.
	t.Setenv("MECATUI_TEST_PLATFORM", "")
	want := PC
	if runtime.GOOS == "darwin" {
		want = Mac
	}
	if got := Current(); got != want {
		t.Fatalf("Current() = %v, want %v (runtime.GOOS=%q)", got, want, runtime.GOOS)
	}
}

func TestPlatformString(t *testing.T) {
	if got := PC.String(); got != "pc" {
		t.Errorf("PC.String() = %q, want %q", got, "pc")
	}
	if got := Mac.String(); got != "mac" {
		t.Errorf("Mac.String() = %q, want %q", got, "mac")
	}
}

func TestScrollKeysMarkingNonMac(t *testing.T) {
	t.Setenv("MECATUI_TEST_PLATFORM", "pc")
	if got := ScrollKeysMarking(); got != "pgup/pgdn" {
		t.Fatalf("ScrollKeysMarking() = %q, want %q", got, "pgup/pgdn")
	}
}

func TestScrollKeysMarkingMac(t *testing.T) {
	t.Setenv("MECATUI_TEST_PLATFORM", "mac")
	got := ScrollKeysMarking()
	if !strings.Contains(got, "pgup/pgdn") {
		t.Errorf("ScrollKeysMarking() = %q, want substring %q", got, "pgup/pgdn")
	}
	if !strings.Contains(got, "fn+↑") {
		t.Errorf("ScrollKeysMarking() = %q, want substring %q", got, "fn+↑")
	}
}
