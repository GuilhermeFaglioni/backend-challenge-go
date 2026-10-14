// Package faultinject lets the test layers crash a process at a chosen point,
// to prove that another instance recovers the work (ARCHITECTURE.md, section
// 18). It exists only in binaries built with the `faultinject` build tag;
// without it Hit compiles to nothing and the FAULT_POINTS variable is ignored.
//
// A point is a named place in the code, reached with Hit. A process exits
// abruptly, as a crash would, when its name is listed in FAULT_POINTS.
package faultinject

// EnvVar holds the comma-separated names of the points that must fire.
const EnvVar = "FAULT_POINTS"

// ExitCode is the status of a process stopped by a fault point, so a test can
// tell it apart from a normal exit (0), a configuration error (1) and a kill.
const ExitCode = 77

// Point names a place where a process can be made to crash.
type Point string

// Points. Business points (between commit and acknowledgement, between publish
// and mark) are added next to the flows they interrupt.
const (
	// HTTPBeforeListen is reached before the HTTP server opens its port.
	HTTPBeforeListen Point = "http_before_listen"
	// HTTPAfterListen is reached right after the HTTP server opens its port.
	HTTPAfterListen Point = "http_after_listen"
)

// Points lists every point. Adding a constant above without adding it here is
// caught by a test, so a point cannot exist in the code and be unknown to the
// `faultinject` build.
func Points() []Point {
	return []Point{HTTPBeforeListen, HTTPAfterListen}
}
