//go:build !faultinject

package faultinject

// Hit does nothing: fault points exist only in `faultinject` builds.
func Hit(Point) {}
