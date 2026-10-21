package integration

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	mysqlDriver "github.com/go-sql-driver/mysql"
	"loreviva.local/loreviva/internal/content"
	store "loreviva.local/loreviva/internal/store/mysql"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
)

const createParticipantSQL = "INSERT INTO integration_world_participants(tenant_id,product_id,world_instance_id,customer_id,service_owner_id,player_subject_id,release_id,creation_receipt,entitlement_id,external_id,enabled,revision) VALUES(?,?,?,?,?,?,?,?,?,?,1,1)"
const createBaselineSQL = "INSERT INTO delivery_streams(tenant_id,product_id,world_instance_id,update_seq,initialization_receipt,projection_version) VALUES(?,?,?,0,?,1)"
const createEntitlementSQL = "UPDATE integration_entitlements SET world_instance_id=?,revision=revision+1 WHERE tenant_id=? AND product_id=? AND customer_id=? AND entitlement_id=? AND player_subject_id=? AND release_id=? AND world_instance_id IS NULL AND revision=? AND enabled=1 AND expires_at>UTC_TIMESTAMP(6) AND expires_at=?"

type creationSQLProbe struct {
	mu                                       sync.Mutex
	mode, world                              string
	fired, opened, closed, stmts, stmtClosed int
	affected                                 int64
	serverErr                                uint16
	gap                                      bool
}
type creationConnector struct {
	inner driver.Connector
	p     *creationSQLProbe
}
type creationConn struct {
	cLeaseDriverConn
	p *creationSQLProbe
}

func (c *creationConnector) Driver() driver.Driver { return c.inner.Driver() }
func (c *creationConnector) Connect(ctx context.Context) (driver.Conn, error) {
	raw, e := c.inner.Connect(ctx)
	if e != nil {
		return raw, e
	}
	c.p.mu.Lock()
	c.p.opened++
	c.p.mu.Unlock()
	v, ok := raw.(cLeaseDriverConn)
	if !ok {
		e = raw.Close()
		c.p.mu.Lock()
		c.p.gap = true
		if e == nil {
			c.p.closed++
		}
		c.p.mu.Unlock()
		return nil, errors.New("unsupported actual driver interfaces")
	}
	return &creationConn{v, c.p}, nil
}
func (c *creationConn) Close() error {
	e := c.cLeaseDriverConn.Close()
	c.p.mu.Lock()
	defer c.p.mu.Unlock()
	if e == nil {
		c.p.closed++
	} else {
		c.p.gap = true
	}
	return e
}
func (c *creationConn) ExecContext(ctx context.Context, q string, a []driver.NamedValue) (driver.Result, error) {
	p := c.p
	p.mu.Lock()
	mode := p.mode
	target := createParticipantSQL
	n := 10
	worldIndex := 2
	if mode == "baseline" {
		target = createBaselineSQL
		n = 4
	}
	if mode == "cas_zero" {
		target = createEntitlementSQL
		n = 9
		worldIndex = 0
	}
	if mode == "" || q != target {
		p.mu.Unlock()
		return c.cLeaseDriverConn.ExecContext(ctx, q, a)
	}
	if p.fired != 0 || len(a) != n || a[worldIndex].Value != p.world {
		p.gap = true
		p.mu.Unlock()
		return c.cLeaseDriverConn.ExecContext(ctx, q, a)
	}
	for i, v := range a {
		if v.Ordinal != i+1 || v.Name != "" {
			p.gap = true
		}
	}
	p.fired++
	p.mode = ""
	p.mu.Unlock()
	effective := append([]driver.NamedValue(nil), a...)
	actualQuery := q
	if mode == "cas_zero" {
		effective[0].Value = nil
		actualQuery = strings.Replace(q, "revision=revision+1", "revision=revision", 1)
	} else {
		actualQuery = "INSRT" + strings.TrimPrefix(q, "INSERT")
	}
	// Faults affect only the original statement's SET/SQL syntax. The exact
	// WHERE authority and deadline arguments remain unchanged on the real DB.
	result, e := c.cLeaseDriverConn.ExecContext(ctx, actualQuery, effective)
	if e == driver.ErrSkip {
		result, e = c.prepared(ctx, actualQuery, effective)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var server *mysqlDriver.MySQLError
	if errors.As(e, &server) {
		p.serverErr = server.Number
	}
	if e == nil {
		if result == nil {
			p.gap = true
		} else {
			var x error
			p.affected, x = result.RowsAffected()
			p.gap = p.gap || x != nil
		}
	}
	return result, e
}
func (c *creationConn) prepared(ctx context.Context, q string, a []driver.NamedValue) (driver.Result, error) {
	stmt, e := c.cLeaseDriverConn.PrepareContext(ctx, q)
	if e != nil {
		return nil, e
	}
	c.p.mu.Lock()
	c.p.stmts++
	c.p.mu.Unlock()
	defer func() {
		e := stmt.Close()
		c.p.mu.Lock()
		defer c.p.mu.Unlock()
		if e == nil {
			c.p.stmtClosed++
		} else {
			c.p.gap = true
		}
	}()
	exec, ok := stmt.(driver.StmtExecContext)
	if !ok || stmt.NumInput() != len(a) {
		c.p.mu.Lock()
		c.p.gap = true
		c.p.mu.Unlock()
		return nil, errors.New("unsupported exact prepared shape")
	}
	return exec.ExecContext(ctx, a)
}
func installCreationProbe(t *testing.T, s *store.Store, world, mode string) *creationSQLProbe {
	t.Helper()
	original := s.DB
	var name string
	if e := original.QueryRow("SELECT DATABASE()").Scan(&name); e != nil {
		t.Fatal(e)
	}
	if !regexp.MustCompile(`^loreviva_test_[a-f0-9]{24}$`).MatchString(name) {
		t.Fatal("not owned synthetic fixture")
	}
	cfg, e := mysqlDriver.ParseDSN(os.Getenv("LOREVIVA_TEST_DSN"))
	if e != nil {
		t.Fatal("invalid private fixture connection")
	}
	if cfg.Net != "tcp" || cfg.Addr != "127.0.0.1:33267" || strings.EqualFold(cfg.User, "root") || cfg.ClientFoundRows {
		t.Fatal("unapproved fixture connection behavior")
	}
	cfg.DBName = name
	cfg.ParseTime = true
	inner, e := mysqlDriver.NewConnector(cfg)
	if e != nil {
		t.Fatal("invalid fixture connector")
	}
	p := &creationSQLProbe{world: world, affected: -1}
	swapped := false
	observed := sql.OpenDB(&creationConnector{inner, p})
	t.Cleanup(func() {
		p.mu.Lock()
		p.mode = ""
		p.mu.Unlock()
		if swapped {
			s.DB = original
		}
		e := observed.Close()
		p.mu.Lock()
		defer p.mu.Unlock()
		t.Logf("creation_probe opened=%d closed=%d stmts=%d stmt_closed=%d affected=%d server_error=%d fired=%d in_use=%d", p.opened, p.closed, p.stmts, p.stmtClosed, p.affected, p.serverErr, p.fired, observed.Stats().InUse)
		if e != nil || p.gap || p.opened != p.closed || p.stmts != p.stmtClosed || observed.Stats().InUse != 0 {
			t.Error("probe cleanup/protocol failure")
		}
	})
	var actual string
	if e = observed.QueryRow("SELECT DATABASE()").Scan(&actual); e != nil || actual != name {
		t.Fatal("fixture identity mismatch")
	}
	if original.Stats().InUse != 0 {
		t.Fatal("original pool not idle")
	}
	s.DB = observed
	swapped = true
	p.mode = mode
	return p
}
func TestIntegrationCreateRealSQLRollback(t *testing.T) {
	for _, mode := range []string{"participant", "baseline", "cas_zero"} {
		t.Run(mode, func(t *testing.T) {
			r, _, ctx, req, _ := integrationCreationFixture(t)
			key := "s66-sql-" + mode
			id := content.Identity("demo-local", "coffee", "s66-service", "createInstance", key)
			p := installCreationProbe(t, r.Store, id, mode)
			_, e := r.World.CreateIntegrationInstance(ctx, key, req)
			if e == nil || integrationWorldRows(t, r.Store, id) != 0 {
				t.Fatal("fault did not atomically reject", e)
			}
			p.mu.Lock()
			fired, affected, server, gap := p.fired, p.affected, p.serverErr, p.gap
			p.mu.Unlock()
			if fired != 1 || gap || mode == "cas_zero" && affected != 0 || mode != "cas_zero" && server != 1064 {
				t.Fatal("not the required real server outcome", fired, affected, server, gap)
			}
			if _, e = r.World.CreateIntegrationInstance(ctx, key, req); e != nil {
				t.Fatal("fault rollback did not preserve original entitlement/key", e)
			}
		})
	}
}
