// Package migrations checks the convention every migration follows: each one has
// an up and a down file, versions are contiguous, and the down file starts with a
// rollback header saying what it undoes, what it loses, what it requires and the
// command to run. The convention is described in the README ("Migrations").
//
// The checking logic is a function over an fs.FS, so it is tested against
// deliberately bad directories as well as against the real one.
package migrations

import (
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"
)

// A field shorter than this cannot say anything useful.
const minFieldLength = 3

// Fields every rollback header must have, in this order.
var headerFields = []string{"Undoes", "Loses", "Requires", "Command"}

var (
	fileName  = regexp.MustCompile(`^(\d{6})_([a-z0-9_]+)\.(up|down)\.sql$`)
	fieldLine = regexp.MustCompile(`^--\s*([A-Za-z]+):\s*(.*)$`)
	continued = regexp.MustCompile(`^--\s{2,}(\S.*)$`)

	// Statements that lose data (DROP TABLE, TRUNCATE...) or protections (dropping a
	// trigger, index, constraint or function, or revoking a privilege).
	destructive = regexp.MustCompile(`(?i)\b(DROP\s+(TABLE|COLUMN|SCHEMA|DATABASE|INDEX|TRIGGER|CONSTRAINT|FUNCTION|POLICY)|TRUNCATE|DELETE\s+FROM|REVOKE)\b`)
	// Ways of writing "nothing is lost", none of which a destructive rollback may use.
	nothing = regexp.MustCompile(`(?i)^(nothing|none|no data|no loss|n/a|not applicable)\b`)

	// SQL comments: block comments, and anything after -- on a line.
	blockComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
	lineComment  = regexp.MustCompile(`--[^\n]*`)
)

// check returns every violation found in a migrations directory, one per line.
func check(fsys fs.FS) []string {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return []string{"cannot read the migrations directory: " + err.Error()}
	}

	type pair struct{ up, down bool }
	versions := map[string]*pair{}
	names := map[string]string{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		m := fileName.FindStringSubmatch(entry.Name())
		if m == nil {
			add("%s: the name must look like 000001_short_name.up.sql or .down.sql", entry.Name())
			continue
		}
		version, name, direction := m[1], m[2], m[3]
		if previous, seen := names[version]; seen && previous != name {
			add("version %s is used by two migrations: %s and %s", version, previous, name)
		}
		names[version] = name
		if versions[version] == nil {
			versions[version] = &pair{}
		}
		if direction == "up" {
			versions[version].up = true
		} else {
			versions[version].down = true
		}
	}

	sorted := make([]string, 0, len(versions))
	for version := range versions {
		sorted = append(sorted, version)
	}
	sort.Strings(sorted)

	for i, version := range sorted {
		if want := fmt.Sprintf("%06d", i+1); version != want {
			add("versions must be contiguous from 000001: expected %s, found %s", want, version)
		}
		p := versions[version]
		base := version + "_" + names[version]
		switch {
		case p.up && !p.down:
			add("%s.up.sql has no %s.down.sql", base, base)
		case p.down && !p.up:
			add("%s.down.sql has no %s.up.sql", base, base)
		}
		if p.down {
			for _, problem := range checkDownFile(fsys, base+".down.sql") {
				add("%s", problem)
			}
		}
	}
	return problems
}

// checkDownFile validates the rollback header of one .down.sql.
func checkDownFile(fsys fs.FS, name string) []string {
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return []string{name + ": " + err.Error()}
	}
	text := string(data)
	var problems []string

	// The header is the block of comment lines at the very top of the file.
	var header []string
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			break
		}
		header = append(header, strings.TrimRight(line, " \t\r"))
	}
	if len(header) == 0 {
		return []string{name + ": the file must start with the rollback header (" + strings.Join(headerFields, ", ") + ")"}
	}

	fields := map[string]string{}
	var order []string
	var current string
	for _, line := range header {
		if m := fieldLine.FindStringSubmatch(line); m != nil && slices.Contains(headerFields, m[1]) {
			current = m[1]
			if _, again := fields[current]; again {
				problems = append(problems, name+": the rollback header field "+current+" appears more than once")
			}
			order = append(order, current)
			fields[current] = strings.TrimSpace(m[2])
			continue
		}
		if m := continued.FindStringSubmatch(line); m != nil && current != "" {
			fields[current] = strings.TrimSpace(fields[current] + " " + m[1])
		}
	}

	for _, field := range headerFields {
		value, present := fields[field]
		switch {
		case !present:
			problems = append(problems, name+": the rollback header lacks the field "+field)
		case len(value) < minFieldLength:
			problems = append(problems, name+": the rollback header field "+field+" is empty")
		}
	}
	if len(problems) == 0 && !slices.Equal(order, headerFields) {
		problems = append(problems, name+": the header fields must come in this order: "+strings.Join(headerFields, ", "))
	}
	if cmd, ok := fields["Command"]; ok && len(cmd) >= minFieldLength && (!strings.Contains(cmd, "migrate") || !strings.Contains(cmd, "down")) {
		problems = append(problems, name+": the Command field must show the migrate down command")
	}

	// A rollback that destroys data or removes protections must say so in the Loses field.
	if destructive.MatchString(stripComments(text)) {
		if loses, ok := fields["Loses"]; ok && nothing.MatchString(loses) {
			problems = append(problems, name+": the file removes data or protections, but Loses says it loses "+loses)
		}
	}
	return problems
}

// stripComments removes SQL comments, so a statement that is only mentioned in one
// is not mistaken for one that runs. It is deliberately simple: a "--" inside a
// string literal would be cut too, which at worst makes the check more lenient on a
// line the reviewer reads anyway.
func stripComments(sql string) string {
	return lineComment.ReplaceAllString(blockComment.ReplaceAllString(sql, ""), "")
}

// --- tests -----------------------------------------------------------------

const goodHeader = `-- Undoes: the first migration.
-- Loses: nothing; it only revokes privileges.
-- Requires: nothing.
-- Command: migrate -path migrations -database "$DATABASE_URL" down 1
`

func dir(files map[string]string) fs.FS {
	m := fstest.MapFS{}
	for name, content := range files {
		m[name] = &fstest.MapFile{Data: []byte(content)}
	}
	return m
}

func TestTheRealMigrationsFollowTheConvention(t *testing.T) {
	require.Empty(t, check(os.DirFS("../../migrations")))
}

func TestAWellFormedDirectoryHasNoProblems(t *testing.T) {
	require.Empty(t, check(dir(map[string]string{
		"000001_first.up.sql":    "SELECT 1;",
		"000001_first.down.sql":  goodHeader + "SELECT 1;",
		"000002_second.up.sql":   "SELECT 2;",
		"000002_second.down.sql": goodHeader + "SELECT 2;",
	})))
}

func TestTheGuardRejectsWhatBreaksTheConvention(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{
			"a down file without a header",
			map[string]string{"000001_a.up.sql": "SELECT 1;", "000001_a.down.sql": "SELECT 1;"},
			"must start with the rollback header",
		},
		{
			"a header that is not at the top",
			map[string]string{"000001_a.up.sql": "SELECT 1;", "000001_a.down.sql": "SELECT 1;\n" + goodHeader},
			"must start with the rollback header",
		},
		{
			"a missing field",
			map[string]string{
				"000001_a.up.sql":   "SELECT 1;",
				"000001_a.down.sql": "-- Undoes: the first migration.\n-- Loses: nothing.\n-- Command: migrate down 1\nSELECT 1;",
			},
			"lacks the field Requires",
		},
		{
			"an empty field",
			map[string]string{
				"000001_a.up.sql":   "SELECT 1;",
				"000001_a.down.sql": "-- Undoes: the first migration.\n-- Loses:\n-- Requires: nothing.\n-- Command: migrate down 1\nSELECT 1;",
			},
			"field Loses is empty",
		},
		{
			"fields out of order",
			map[string]string{
				"000001_a.up.sql":   "SELECT 1;",
				"000001_a.down.sql": "-- Loses: nothing.\n-- Undoes: the first migration.\n-- Requires: nothing.\n-- Command: migrate down 1\nSELECT 1;",
			},
			"must come in this order",
		},
		{
			"a command that is not the migrate command",
			map[string]string{
				"000001_a.up.sql":   "SELECT 1;",
				"000001_a.down.sql": "-- Undoes: the first migration.\n-- Loses: nothing.\n-- Requires: nothing.\n-- Command: run the thing\nSELECT 1;",
			},
			"must show the migrate down command",
		},
		{
			"an up file with no down file",
			map[string]string{"000001_a.up.sql": "SELECT 1;"},
			"has no 000001_a.down.sql",
		},
		{
			"a down file with no up file",
			map[string]string{"000001_a.down.sql": goodHeader + "SELECT 1;"},
			"has no 000001_a.up.sql",
		},
		{
			"a gap in the versions",
			map[string]string{
				"000001_a.up.sql": "SELECT 1;", "000001_a.down.sql": goodHeader,
				"000003_c.up.sql": "SELECT 1;", "000003_c.down.sql": goodHeader,
			},
			"expected 000002, found 000003",
		},
		{
			"two migrations with one version",
			map[string]string{
				"000001_a.up.sql": "SELECT 1;", "000001_a.down.sql": goodHeader,
				"000001_b.up.sql": "SELECT 1;", "000001_b.down.sql": goodHeader,
			},
			"used by two migrations",
		},
		{
			"a file name that does not follow the pattern",
			map[string]string{"1_a.up.sql": "SELECT 1;"},
			"the name must look like",
		},
		{
			"a rollback that drops a table but claims to lose nothing",
			map[string]string{
				"000001_a.up.sql":   "SELECT 1;",
				"000001_a.down.sql": goodHeader + "DROP TABLE wallet_ledger_entries;",
			},
			"removes data or protections, but Loses says",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			problems := check(dir(tt.files))
			require.NotEmpty(t, problems, "the guard accepted a directory that breaks the convention")
			require.Contains(t, strings.Join(problems, "\n"), tt.want)
		})
	}
}

// Protections are lost too, not only data: dropping a trigger or a constraint, or
// revoking a privilege, weakens the guarantees the schema gives, and the header must
// name that instead of claiming nothing is lost.
func TestARollbackThatRemovesProtectionsCannotClaimToLoseNothing(t *testing.T) {
	for _, statement := range []string{
		"DROP TRIGGER ledger_immutable ON wallet_ledger_entries;",
		"DROP INDEX one_reversal_per_reference;",
		"ALTER TABLE wallets DROP CONSTRAINT balance_not_negative;",
		"DROP FUNCTION refuse_ledger_changes();",
		"REVOKE UPDATE, DELETE ON wallet_ledger_entries FROM wagering_app;",
		"drop trigger if exists t on x;",
	} {
		t.Run(statement, func(t *testing.T) {
			problems := check(dir(map[string]string{
				"000001_a.up.sql":   "SELECT 1;",
				"000001_a.down.sql": goodHeader + statement,
			}))
			require.Contains(t, strings.Join(problems, "\n"), "Loses says")
		})
	}
}

func TestNothingHasSeveralSpellings(t *testing.T) {
	for _, loses := range []string{"nothing", "None.", "no data", "n/a", "N/A", "no loss", "not applicable"} {
		t.Run(loses, func(t *testing.T) {
			header := "-- Undoes: the first migration.\n-- Loses: " + loses + "\n-- Requires: nothing.\n-- Command: make migrate-down\n"
			problems := check(dir(map[string]string{
				"000001_a.up.sql":   "SELECT 1;",
				"000001_a.down.sql": header + "DROP TABLE a;",
			}))
			require.Contains(t, strings.Join(problems, "\n"), "Loses says", "Loses: %s", loses)
		})
	}
}

// Statements that are only mentioned in a comment are not executed, so they must not
// make an honest "Loses: nothing" a violation.
func TestCommentedOutStatementsAreNotDestructive(t *testing.T) {
	for name, body := range map[string]string{
		"a trailing comment": "SELECT 1; -- DROP TABLE x",
		"a block comment":    "/* DROP TABLE x */ SELECT 1;",
		"a multi-line block": "/*\n DROP TRIGGER t ON x;\n REVOKE ALL ON x FROM y;\n*/\nSELECT 1;",
	} {
		t.Run(name, func(t *testing.T) {
			require.Empty(t, check(dir(map[string]string{
				"000001_a.up.sql":   "SELECT 1;",
				"000001_a.down.sql": goodHeader + body,
			})))
		})
	}
}

func TestARepeatedFieldIsReportedAsSuch(t *testing.T) {
	header := goodHeader + "-- Loses: something else.\n"
	problems := check(dir(map[string]string{
		"000001_a.up.sql":   "SELECT 1;",
		"000001_a.down.sql": header + "SELECT 1;",
	}))
	require.Contains(t, strings.Join(problems, "\n"), "field Loses appears more than once")
}

func TestADestructiveRollbackThatSaysSoIsAccepted(t *testing.T) {
	header := `-- Undoes: the creation of the ledger table.
-- Loses: ALL ledger entries, that is, the financial history. Use only before real data exists.
-- Requires: nothing.
-- Command: migrate -path migrations -database "$DATABASE_URL" down 1
`
	require.Empty(t, check(dir(map[string]string{
		"000001_ledger.up.sql":   "CREATE TABLE wallet_ledger_entries (id int);",
		"000001_ledger.down.sql": header + "DROP TABLE wallet_ledger_entries;",
	})))
}

func TestAHeaderFieldMayContinueOnTheNextLines(t *testing.T) {
	header := `-- Undoes: the creation of the table and
--   its indexes.
-- Loses: every row of the table.
-- Requires: nothing.
-- Command: migrate -path migrations -database "$DATABASE_URL" down 1
`
	require.Empty(t, check(dir(map[string]string{
		"000001_t.up.sql":   "CREATE TABLE t (id int);",
		"000001_t.down.sql": header + "DROP TABLE t;",
	})))
}
