package harness

import (
	"context"
	"fmt"
	"net"
	"time"

	toxiclient "github.com/Shopify/toxiproxy/v2/client"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	tctoxiproxy "github.com/testcontainers/testcontainers-go/modules/toxiproxy"
	"github.com/testcontainers/testcontainers-go/network"
)

const (
	// PostgresImage is the pinned engine. SPEC.md §3.3: PostgreSQL 19 is in
	// beta, GA is projected rather than committed, and SQL/PGQ — which the
	// property graph is written in — exists in no earlier release.
	PostgresImage = "postgres:19beta2"

	// ToxiproxyImage is the pinned proxy for failure-matrix row F3.
	ToxiproxyImage = "ghcr.io/shopify/toxiproxy:2.12.0"

	dbName     = "agentiq"
	dbUser     = "agentiq"
	dbPassword = "agentiq"

	// networkAlias is the name the proxy resolves the database by. It is a
	// container alias on a user-defined network, so it only exists when the
	// fixture was built [WithProxy].
	networkAlias = "postgres"

	// snapshotName is the restore point taken once `dbos.*` exists.
	snapshotName = "agentiq_clean"

	// firstProxiedPort mirrors the toxiproxy module's own unexported constant:
	// the first listen port it assigns to a declared proxy. The fixture
	// declares exactly one, so this is its listen port.
	firstProxiedPort = 8666
)

// Postgres is a running PostgreSQL 19 fixture.
//
// The zero value is not usable; build one with [StartPostgres].
type Postgres struct {
	Container *postgres.PostgresContainer

	// DSN reaches the database directly.
	DSN string

	// ProxiedDSN reaches it through toxiproxy, and is empty unless the
	// fixture was built [WithProxy]. Failure-matrix row F3 is the only caller:
	// everything else wants the shortest path to the database, because a proxy
	// in front of every test is one more thing that can be the reason a test
	// is flaky.
	ProxiedDSN string

	proxy     *toxiclient.Proxy
	snapshot  bool
	tb        testingTB
	proxyName string
}

type postgresOptions struct {
	proxy bool
}

// PostgresOption configures [StartPostgres].
type PostgresOption func(*postgresOptions)

// WithProxy puts a toxiproxy between the client and the database and populates
// [Postgres.ProxiedDSN]. It costs a user-defined Docker network and a second
// container, so it is opt-in.
func WithProxy() PostgresOption { return func(o *postgresOptions) { o.proxy = true } }

// StartPostgres runs PostgreSQL 19 and registers its termination with tb.
//
// The wait strategy is [postgres.BasicWaitStrategies], which waits for the
// readiness log line *twice* because PostgreSQL restarts once after initdb
// (SPEC.md §14.2). Omitting it is the primary source of flaky startup on macOS
// and Windows runners, and the failure it produces — a connection refused on
// the first query — reads like a bug in the code under test.
func StartPostgres(ctx context.Context, tb testingTB, opts ...PostgresOption) *Postgres {
	tb.Helper()

	// Before anything starts: Snapshot and Restore below go through
	// database/sql, and testcontainers falls back to `docker exec psql`
	// without saying so when the driver is missing.
	RequireSQLDriver(tb, SQLDriverName)

	var cfg postgresOptions
	for _, opt := range opts {
		opt(&cfg)
	}

	moduleOpts := []testcontainers.ContainerCustomizer{
		postgres.WithDatabase(dbName),
		postgres.WithUsername(dbUser),
		postgres.WithPassword(dbPassword),
		postgres.WithSQLDriver(SQLDriverName),
		// SPEC.md §14.2 writes this as
		// `testcontainers.WithWaitStrategy(postgres.BasicWaitStrategies())`.
		// In testcontainers-go v0.43.0 BasicWaitStrategies is already a
		// testcontainers.CustomizeRequestOption, not a wait.Strategy, so
		// wrapping it does not compile. The behaviour §14.2 describes is
		// unchanged — it is the same double readiness check.
		postgres.BasicWaitStrategies(),
	}

	var nw *testcontainers.DockerNetwork
	if cfg.proxy {
		var err error
		nw, err = network.New(ctx)
		if err != nil {
			tb.Fatalf("harness: create docker network: %v", err)
		}
		tb.Cleanup(func() { _ = nw.Remove(context.WithoutCancel(ctx)) })
		moduleOpts = append(moduleOpts, network.WithNetwork([]string{networkAlias}, nw))
	}

	container, err := postgres.Run(ctx, PostgresImage, moduleOpts...)
	if err != nil {
		tb.Fatalf("harness: start %s: %v", PostgresImage, err)
	}
	tb.Cleanup(func() {
		_ = testcontainers.TerminateContainer(container, testcontainers.StopTimeout(30*time.Second))
	})

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		tb.Fatalf("harness: connection string: %v", err)
	}

	pg := &Postgres{Container: container, DSN: dsn, tb: tb}

	if cfg.proxy {
		pg.startProxy(ctx, tb, nw)
	}
	return pg
}

// startProxy runs toxiproxy on the database's network and fills in ProxiedDSN.
func (p *Postgres) startProxy(ctx context.Context, tb testingTB, nw *testcontainers.DockerNetwork) {
	tb.Helper()

	p.proxyName = "postgres"
	upstream := net.JoinHostPort(networkAlias, "5432")

	container, err := tctoxiproxy.Run(ctx, ToxiproxyImage,
		network.WithNetwork([]string{"toxiproxy"}, nw),
		tctoxiproxy.WithProxy(p.proxyName, upstream),
	)
	if err != nil {
		tb.Fatalf("harness: start %s: %v", ToxiproxyImage, err)
	}
	tb.Cleanup(func() {
		_ = testcontainers.TerminateContainer(container, testcontainers.StopTimeout(30*time.Second))
	})

	// ProxiedEndpoint is keyed by the proxy's *listen* port, not the upstream
	// port it forwards to. The module assigns listen ports from 8666 upwards in
	// the order proxies were declared, and does not export that base — so
	// asking for 5432 here returns "port not found", which reads like a
	// container that failed to start.
	host, port, err := container.ProxiedEndpoint(firstProxiedPort)
	if err != nil {
		tb.Fatalf("harness: proxied endpoint: %v", err)
	}
	p.ProxiedDSN = fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable",
		dbUser, dbPassword, net.JoinHostPort(host, port), dbName)

	uri, err := container.URI(ctx)
	if err != nil {
		tb.Fatalf("harness: toxiproxy control URI: %v", err)
	}
	proxy, err := toxiclient.NewClient(uri).Proxy(p.proxyName)
	if err != nil {
		tb.Fatalf("harness: look up proxy %q: %v", p.proxyName, err)
	}
	p.proxy = proxy
}

// Partition cuts every connection through the proxy for d, then heals it.
//
// This is failure-matrix row F3's mechanism. Disable is a hard cut rather than
// a latency toxic on purpose: latency makes a slow test, a cut makes a test of
// what the code does when the database is *gone*, which is the case the retry
// logic exists for.
func (p *Postgres) Partition(ctx context.Context, d time.Duration) error {
	if p.proxy == nil {
		return fmt.Errorf("harness: no proxy; build the fixture with harness.WithProxy()")
	}
	p.proxy.Enabled = false
	if err := p.proxy.Save(); err != nil {
		return fmt.Errorf("harness: cut the connection: %w", err)
	}

	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}

	p.proxy.Enabled = true
	if err := p.proxy.Save(); err != nil {
		return fmt.Errorf("harness: heal the connection: %w", err)
	}
	return nil
}

// Bounce stops and restarts the database container, which is failure-matrix row
// F2's mechanism.
//
// The container keeps its volume, so this is a restart rather than a fresh
// database — the workflow has to still be there when it comes back, which is
// the whole assertion.
func (p *Postgres) Bounce(ctx context.Context) error {
	timeout := 30 * time.Second
	if err := p.Container.Stop(ctx, &timeout); err != nil {
		return fmt.Errorf("harness: stop the database: %w", err)
	}
	if err := p.Container.Start(ctx); err != nil {
		return fmt.Errorf("harness: start the database: %w", err)
	}

	// The published port changes across a stop/start, so anything holding the
	// old DSN is now pointed at nothing. Callers that need the new one read
	// [Postgres.DSN] again; the worker under test does not, which is the
	// point — it reconnects through a DSN the fixture keeps stable only when
	// the proxy is in front. Refresh it so the *observer* can still look.
	dsn, err := p.Container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return fmt.Errorf("harness: connection string after restart: %w", err)
	}
	p.DSN = dsn
	return nil
}

// Snapshot records the current database as the restore point. It is taken once
// `dbos.*` exists, so [Postgres.Restore] returns a database that is clean but
// migrated — which is what "a clean PostgreSQL 19 database" means for a system
// whose schema is owned by somebody else (SPEC.md §2.2).
func (p *Postgres) Snapshot(ctx context.Context) error {
	if err := p.Container.Snapshot(ctx, postgres.WithSnapshotName(snapshotName)); err != nil {
		return fmt.Errorf("harness: snapshot: %w", err)
	}
	p.snapshot = true
	return nil
}

// Restore returns the database to the state [Postgres.Snapshot] recorded.
//
// Every connection to the database must be closed first: Restore drops it, and
// PostgreSQL will not drop a database that has sessions attached. A worker left
// running here fails the restore, not the scenario, which is why the durable
// suite kills its worker before resetting.
func (p *Postgres) Restore(ctx context.Context) error {
	if !p.snapshot {
		return fmt.Errorf("harness: no snapshot taken")
	}
	if err := p.Container.Restore(ctx); err != nil {
		return fmt.Errorf("harness: restore: %w", err)
	}
	return nil
}
