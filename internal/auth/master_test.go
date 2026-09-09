package auth

import (
	"strings"
	"testing"

	"github.com/DiegohNY/costlane/internal/obs"
)

func TestMasterKeyMatchesItself(t *testing.T) {
	master := obs.Secret("master-key-of-at-least-32-characters")
	if !VerifyMaster(master, master) {
		t.Error("the configured master key must verify")
	}
}

func TestMasterKeyRejectsAnythingElse(t *testing.T) {
	master := obs.Secret("master-key-of-at-least-32-characters")

	for _, candidate := range []string{
		"",
		"wrong",
		"master-key-of-at-least-32-character", // one short
		"master-key-of-at-least-32-charactersX",
		"MASTER-KEY-OF-AT-LEAST-32-CHARACTERS",
		strings.Repeat("x", 1000), // far longer, must not panic or match
	} {
		if VerifyMaster(master, obs.Secret(candidate)) {
			t.Errorf("candidate %q must not verify", candidate)
		}
	}
}

// An unconfigured master key must never verify, or a deployment that forgot
// to set it would accept an empty Authorization header as administrative.
func TestUnsetMasterKeyVerifiesNothing(t *testing.T) {
	for _, candidate := range []string{"", "anything"} {
		if VerifyMaster(obs.Secret(""), obs.Secret(candidate)) {
			t.Errorf("an unset master key must reject %q", candidate)
		}
	}
}
