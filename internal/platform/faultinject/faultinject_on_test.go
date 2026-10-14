//go:build faultinject

package faultinject

import (
	"strings"
	"testing"
)

func TestHitExitsOnlyForTheActivePoint(t *testing.T) {
	var exitedWith []int
	restore := setForTest(t, map[Point]bool{HTTPAfterListen: true}, func(code int) { exitedWith = append(exitedWith, code) })
	defer restore()

	Hit(HTTPAfterListen)
	if len(exitedWith) != 1 || exitedWith[0] != ExitCode {
		t.Fatalf("expected one exit with code %d, got %v", ExitCode, exitedWith)
	}

	exitedWith = nil
	Hit(HTTPBeforeListen)
	if len(exitedWith) != 0 {
		t.Fatalf("an inactive point must not exit, got %v", exitedWith)
	}
}

func TestParseAcceptsKnownPointsAndIgnoresBlanks(t *testing.T) {
	active, err := parse(" http_after_listen , ,http_before_listen ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !active[HTTPAfterListen] || !active[HTTPBeforeListen] || len(active) != 2 {
		t.Fatalf("unexpected active set: %v", active)
	}

	empty, err := parse("")
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty input must activate nothing, got %v, %v", empty, err)
	}
}

// A typo in FAULT_POINTS would otherwise mean the fault never fires and the
// test it belongs to passes without testing anything.
func TestParseRejectsUnknownPoints(t *testing.T) {
	_, err := parse("http_after_listen,http_after_lissten")
	if err == nil || !strings.Contains(err.Error(), "http_after_lissten") {
		t.Fatalf("expected an error naming the unknown point, got %v", err)
	}
}

func TestHitPanicsOnAnUnregisteredPoint(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("calling Hit with a point that is not registered must panic")
		}
	}()
	Hit(Point("not_registered"))
}

// setForTest swaps the active set and the exit function; it returns a function
// that restores them.
func setForTest(t *testing.T, points map[Point]bool, exitFn func(int)) (restore func()) {
	t.Helper()
	previousActive, previousExit := active, exit
	active, exit = points, exitFn
	return func() { active, exit = previousActive, previousExit }
}
