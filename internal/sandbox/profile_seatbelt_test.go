package sandbox

import (
	"strings"
	"testing"
)

func TestSeatbeltProfile_DenyDefaultAndNetwork(t *testing.T) {
	profile := seatbeltProfile("/tmp/session-work")

	for _, want := range []string{
		"(deny default)",
		"(deny network*)",
		"/tmp/session-work",
	} {
		if !strings.Contains(profile, want) {
			t.Errorf("profile missing %q:\n%s", want, profile)
		}
	}
}

func TestSeatbeltProfile_WorkdirReadWrite(t *testing.T) {
	profile := seatbeltProfile("/tmp/session-work")

	if !strings.Contains(profile, `(allow file-read* file-write*`) {
		t.Errorf("profile missing combined read+write allow for workdir:\n%s", profile)
	}
}

// TestSeatbeltProfile_NoSecretLeak asserts the profile generator never
// embeds process-environment content: it is a pure function of workdir, so a
// secret sitting in the environment (as it would be for any real gofer
// process reading API keys, tokens, etc.) must never appear in the generated
// SBPL text.
func TestSeatbeltProfile_NoSecretLeak(t *testing.T) {
	t.Setenv("GOFER_TEST_SECRET", "super-secret-token-do-not-leak")

	profile := seatbeltProfile("/tmp/session-work")

	if strings.Contains(profile, "super-secret-token-do-not-leak") {
		t.Fatalf("profile leaked env secret value:\n%s", profile)
	}
	if strings.Contains(profile, "GOFER_TEST_SECRET") {
		t.Fatalf("profile leaked env var name:\n%s", profile)
	}
}
