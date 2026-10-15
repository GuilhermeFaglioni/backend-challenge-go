//go:build multiinstance

package multiinstance

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/platform/faultinject"
	"github.com/GuilhermeFaglioni/backend-challenge-go/test/infra"
)

const (
	startTimeout = 90 * time.Second // a race-instrumented process boots slower
	stopTimeout  = 30 * time.Second
)

// freeAddrs returns n distinct loopback addresses. All the listeners are open at
// once before any is closed, so the same port cannot be handed out twice. A
// process can still lose the port to an unrelated program between the close and
// its own bind; that window is small and is not worth a retry (decision 13).
func freeAddrs(t *testing.T, n int) []string {
	t.Helper()
	var (
		listeners []net.Listener
		addrs     []string
	)
	for range n {
		ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
		require.NoError(t, err)
		listeners = append(listeners, ln)
		addrs = append(addrs, ln.Addr().String())
	}
	for _, ln := range listeners {
		require.NoError(t, ln.Close())
	}
	return addrs
}

// environment is the process configuration, the same variables the compose
// file sets: the application is configured only through them. The database
// connection carries the instance name as application_name, so PostgreSQL can
// tell which process owns which connection.
func environment(name, addr string, db infra.Database, extra map[string]string) map[string]string {
	env := infra.HermeticAWSEnv()
	for key, value := range map[string]string{
		"INSTANCE_ID":        name,
		"HTTP_ADDR":          addr,
		"LOG_LEVEL":          "info",
		"SHUTDOWN_TIMEOUT":   stopTimeout.String(),
		"DATABASE_URL":       db.AppDSN + "&application_name=" + name,
		"DATABASE_MAX_CONNS": "4",
		"AWS_REGION":         "us-east-1",
		"AWS_ENDPOINT_URL":   ls.Endpoint,
		"SQS_INPUT_QUEUE":    "wager-transactions.fifo",
		"SQS_EVENTS_QUEUE":   "wager-events.fifo",
		// The application discovers the identity provider's keys when it starts.
		"OIDC_ISSUER_URL": kc.IssuerURL(),
		"OIDC_AUDIENCE":   "wallet-api",
		// Data written by the instrumented binary when it exits.
		"GOCOVERDIR": coverDir,
	} {
		env[key] = value
	}
	for key, value := range extra {
		env[key] = value
	}
	return env
}

// lifetime returns a context that outlives the test body and ends only after
// the instance cleanups have run, so a process is stopped by its cleanup (which
// lets it shut down and write coverage) and not killed by the end of the test.
func lifetime(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
	t.Cleanup(cancel) // registered first, so it runs after the instance cleanups
	return ctx
}

var unsafeInFileName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// instanceSeq makes every started instance's log file unique. t.Name() repeats
// across -count iterations, and a later passing iteration must not overwrite
// the log of an earlier failing one.
var instanceSeq atomic.Int64

// track registers what must happen to an instance when the test ends: it is
// stopped, and the test fails if the process reported a data race.
func track(t *testing.T, instance *infra.Instance) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), stopTimeout)
		defer cancel()
		// Politely first, so the process flushes its coverage; then for sure.
		_, _ = instance.Terminate(ctx)
		instance.Kill()
		// The binary is built with the race detector, which reports a data race
		// on the process's stderr when it happens: any test using the instance
		// fails if the application raced, whatever the test was about.
		if strings.Contains(instance.Logs.String(), "WARNING: DATA RACE") {
			t.Errorf("%s reported a data race:\n%s", instance.Name, instance.Logs.String())
		}
	})
}

// startInstance launches one process. Its output is saved for CI if the run
// fails (see infra.RunSuite), under a name that is unique across tests.
func startInstance(t *testing.T, name, addr string, db infra.Database, extra map[string]string) *infra.Instance {
	t.Helper()
	instance, err := infra.StartInstance(lifetime(t), binary, infra.InstanceConfig{
		Name:    name,
		LogName: unsafeInFileName.ReplaceAllString(t.Name(), "_") + "-" + name + "-" + strconv.FormatInt(instanceSeq.Add(1), 10),
		Addr:    addr,
		Env:     environment(name, addr, db, extra),
	})
	require.NoError(t, err)
	track(t, instance)
	return instance
}

func waitReady(t *testing.T, instances ...*infra.Instance) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), startTimeout)
	defer cancel()
	for _, instance := range instances {
		require.NoError(t, instance.WaitReady(ctx))
	}
}

// startCluster runs n instances against one freshly migrated database, as the
// compose file runs three against one PostgreSQL.
func startCluster(t *testing.T, n int) (instances []*infra.Instance, db infra.Database) {
	t.Helper()
	db = pg.NewDatabase(t.Context(), t)
	require.NoError(t, infra.Migrate(db.OwnerDSN))
	for i, addr := range freeAddrs(t, n) {
		instances = append(instances, startInstance(t, "instance-"+strconv.Itoa(i+1), addr, db, nil))
	}
	waitReady(t, instances...)
	return instances, db
}

func TestThreeIndependentInstancesBecomeReady(t *testing.T) {
	instances, db := startCluster(t, 3)

	ctx, cancel := context.WithTimeout(t.Context(), startTimeout)
	defer cancel()
	pids := map[int]bool{}
	for _, instance := range instances {
		pids[instance.Pid()] = true
		require.NoError(t, instance.WaitMessage(ctx, "http server listening"))
		require.Contains(t, instance.Logs.String(), `"instanceId":"`+instance.Name+`"`, "each process identifies itself")
	}
	require.Len(t, pids, 3, "three separate operating-system processes")

	// Independent also means separate connections: PostgreSQL sees each process
	// as its own client, not three views of one pool.
	owner, err := pgx.Connect(ctx, db.OwnerDSN)
	require.NoError(t, err)
	defer func() { _ = owner.Close(ctx) }()
	rows, err := owner.Query(ctx, `SELECT application_name, count(*) FROM pg_stat_activity
		WHERE datname = current_database() AND usename = 'wagering_app' GROUP BY 1`)
	require.NoError(t, err)
	connections, err := pgx.CollectRows(rows, pgx.RowToMap)
	require.NoError(t, err)
	owners := map[string]int64{}
	for _, row := range connections {
		owners[row["application_name"].(string)] = row["count"].(int64)
	}
	for _, instance := range instances {
		require.GreaterOrEqual(t, owners[instance.Name], int64(1), "%s holds no connection of its own (seen: %v)", instance.Name, owners)
	}
}

// probe checks a set of instances over and over until stopped, recording every
// moment one of them was not ready. It lets a test assert "stayed ready the
// whole time" across an event instead of sampling after a fixed delay.
type probe struct {
	rounds   atomic.Int64
	mu       sync.Mutex
	failures []string
	stop     chan struct{}
	done     chan struct{}
}

func startProbe(ctx context.Context, instances []*infra.Instance) *probe {
	p := &probe{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(p.done)
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-p.stop:
				return
			case <-ticker.C:
				for _, instance := range instances {
					if !instance.Ready(ctx) {
						p.mu.Lock()
						p.failures = append(p.failures, instance.Name+" was not ready")
						p.mu.Unlock()
					}
				}
				p.rounds.Add(1)
			}
		}
	}()
	return p
}

// waitRounds blocks until the probe has completed at least n more rounds.
func (p *probe) waitRounds(ctx context.Context, n int64) error {
	target := p.rounds.Load() + n
	for p.rounds.Load() < target {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	return nil
}

func (p *probe) finish() []string {
	close(p.stop)
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.failures)
}

func TestKilledInstanceDoesNotAffectTheOthersAndComesBack(t *testing.T) {
	instances, _ := startCluster(t, 3)
	victim, survivors := instances[0], instances[1:]

	// The survivors are watched from before the kill until the replacement is
	// ready, so "stayed ready" covers the whole event.
	watch := startProbe(t.Context(), survivors)
	require.NoError(t, watch.waitRounds(t.Context(), 3), "probe never ran before the kill")

	victim.Kill()
	require.False(t, victim.Running())
	require.NoError(t, watch.waitRounds(t.Context(), 5), "probe stopped running after the kill")

	// The same address comes back with a new process.
	ctx := lifetime(t)
	reborn, err := victim.Restart(ctx)
	require.NoError(t, err)
	track(t, reborn)
	waitReady(t, reborn)
	require.Equal(t, victim.Addr, reborn.Addr)
	require.NotEqual(t, victim.Pid(), reborn.Pid())

	require.Empty(t, watch.finish(), "a survivor stopped being ready while %s was down", victim.Name)
	for _, instance := range append(survivors, reborn) {
		require.True(t, instance.Ready(t.Context()), "%s is not ready", instance.Name)
	}
}

// startupPoints are reached while the process boots; every other point is reached
// by the work of a running instance and has its own recovery test.
var startupPoints = map[faultinject.Point]bool{
	faultinject.HTTPBeforeListen: true,
	faultinject.HTTPAfterListen:  true,
}

// messagePoints are reached by handling a queue message.
var messagePoints = map[faultinject.Point]bool{
	faultinject.ConsumerAfterCommit: true,
}

// waitPoints are reached by an operation waiting for its Reference.
var waitPoints = map[faultinject.Point]bool{
	faultinject.PendingAfterCommit:  true,
	faultinject.PendingAfterReserve: true,
}

// One subtest per point: each fails if that point's Hit call is removed from the
// code, so every point is proven to be wired, not just one of them.
func TestFaultPointCrashesTheProcessAtThatPoint(t *testing.T) {
	for _, point := range faultinject.Points() {
		t.Run(string(point), func(t *testing.T) {
			if messagePoints[point] {
				crashesWhenAMessageArrives(t, point)
				return
			}
			if waitPoints[point] {
				crashesWhenAWaitArrives(t, point)
				return
			}
			if !startupPoints[point] {
				crashesWhenItsWorkArrives(t, point)
				return
			}
			healthy, db := startCluster(t, 1)
			crashing := startInstance(t, "crashing", freeAddrs(t, 1)[0], db, map[string]string{faultinject.EnvVar: string(point)})

			ctx, cancel := context.WithTimeout(t.Context(), startTimeout)
			defer cancel()
			require.NoError(t, crashing.WaitMessage(ctx, "fault point reached: "+string(point)))
			code, err := crashing.WaitExit(ctx)
			require.NoError(t, err)
			require.Equal(t, faultinject.ExitCode, code, "exit status of a fault point, not of a normal stop or a config error")

			// It never got to the end of startup: "http server listening" is
			// logged after both points.
			require.NotContains(t, crashing.Logs.Messages(), "http server listening")
			require.True(t, healthy[0].Ready(t.Context()), "an unrelated instance must not notice")
		})
	}
}

// A typo in the fault list would make the fault silently never fire, and the
// recovery test it belongs to would pass without testing anything.
func TestUnknownFaultPointRefusesToStart(t *testing.T) {
	_, db := startCluster(t, 1)
	bad := startInstance(t, "typo", freeAddrs(t, 1)[0], db, map[string]string{faultinject.EnvVar: "http_after_lissten"})

	ctx, cancel := context.WithTimeout(t.Context(), startTimeout)
	defer cancel()
	code, err := bad.WaitExit(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, code)
	require.Contains(t, bad.Logs.String(), `unknown fault point "http_after_lissten"`)
}

func TestSIGTERMStopsGracefullyAndWritesUsableCoverage(t *testing.T) {
	instances, _ := startCluster(t, 1)
	instance := instances[0]
	pid := instance.Pid()

	ctx, cancel := context.WithTimeout(t.Context(), stopTimeout)
	defer cancel()
	code, err := instance.Terminate(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, code, "a SIGTERM is a normal shutdown; logs:\n%s", instance.Logs.String())

	// The real main path (signal handling, Fx stop hooks) ran: the server stops
	// before the pool it uses is closed.
	msgs := instance.Logs.Messages()
	httpStopped := slices.Index(msgs, "http server stopped")
	poolClosed := slices.Index(msgs, "postgres pool closed")
	require.NotEqual(t, -1, httpStopped, "missing 'http server stopped' in %v", msgs)
	require.NotEqual(t, -1, poolClosed, "missing 'postgres pool closed' in %v", msgs)
	require.Less(t, httpStopped, poolClosed)
	// The outbox publisher stops after the server (nothing writes new events) and
	// before the pool and the SQS client it uses are closed.
	publisherStopped := slices.Index(msgs, "outbox publisher stopped")
	require.NotEqual(t, -1, publisherStopped, "missing 'outbox publisher stopped' in %v", msgs)
	require.Less(t, httpStopped, publisherStopped)
	require.Less(t, publisherStopped, poolClosed)
	require.Less(t, publisherStopped, slices.Index(msgs, "sqs client closed"))

	// The coverage this process wrote must exist under its own pid (other tests
	// share the directory) and be readable by the tool the combined report uses.
	entries, err := os.ReadDir(coverDir)
	require.NoError(t, err)
	var ownCounters, meta bool
	for _, e := range entries {
		ownCounters = ownCounters || (strings.HasPrefix(e.Name(), "covcounters.") && strings.Contains(e.Name(), "."+strconv.Itoa(pid)+"."))
		meta = meta || strings.HasPrefix(e.Name(), "covmeta.")
	}
	require.True(t, ownCounters, "pid %d wrote no coverage counters in %s (entries: %v)", pid, coverDir, entries)
	require.True(t, meta, "no coverage metadata in %s", coverDir)

	profile := filepath.Join(t.TempDir(), "profile.txt")
	out, err := exec.CommandContext(t.Context(), "go", "tool", "covdata", "textfmt", "-i="+coverDir, "-o="+profile).CombinedOutput() //nolint:gosec // fixed arguments and a directory this run created
	require.NoError(t, err, "covdata could not read the coverage data: %s", out)
	text, err := os.ReadFile(profile) //nolint:gosec // a file this test just created in its own temp dir
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(string(text), "mode: atomic"), "unexpected profile header: %.40s", text)
	// Startup ran code in internal/app, so some of it must be reported as executed.
	covered := regexp.MustCompile(`internal/app/app\.go:\S+ \d+ [1-9]\d*\n`)
	require.Regexp(t, covered, string(text), "the profile reports nothing executed in internal/app")
}
