//go:build faultinject

package faultinject

import (
	"fmt"
	"os"
	"strings"
)

var registered = func() map[Point]bool {
	points := map[Point]bool{}
	for _, p := range Points() {
		points[p] = true
	}
	return points
}()

// exit is a variable so a unit test can observe it instead of dying.
var exit = os.Exit

var active = func() map[Point]bool {
	points, err := parse(os.Getenv(EnvVar))
	if err != nil {
		// Refuse to start: a typo would make the fault silently never fire.
		fmt.Fprintf(os.Stderr, "faultinject: %v\n", err)
		os.Exit(1)
	}
	return points
}()

// parse reads a comma-separated list of point names and rejects unknown ones.
func parse(list string) (map[Point]bool, error) {
	points := map[Point]bool{}
	for _, name := range strings.Split(list, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if !registered[Point(name)] {
			return nil, fmt.Errorf("unknown fault point %q in %s", name, EnvVar)
		}
		points[Point(name)] = true
	}
	return points, nil
}

// Hit crashes the process, with no cleanup, if the point is active. Calling it
// with a point nobody registered is a programming error and panics.
func Hit(p Point) {
	if !registered[p] {
		panic(fmt.Sprintf("faultinject: point %q is not registered", p))
	}
	if active[p] {
		// Written straight to stderr: the logger may be buffered, and the test
		// that watches for this line must see it before the process is gone.
		fmt.Fprintf(os.Stderr, "fault point reached: %s\n", p)
		exit(ExitCode)
	}
}
