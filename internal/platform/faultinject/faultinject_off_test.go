//go:build !faultinject

package faultinject

import "testing"

// Production binaries must ignore fault points entirely, even if the
// environment asks for one: a leftover FAULT_POINTS must never kill a process.
func TestHitDoesNothingWithoutTheBuildTag(t *testing.T) {
	t.Setenv(EnvVar, string(HTTPAfterListen))

	// Reaching the end of the test is the assertion: an active point would exit
	// the test binary.
	Hit(HTTPAfterListen)
}
