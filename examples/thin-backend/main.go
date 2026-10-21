// Command thin-backend is the bounded synthetic customer delivery example.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	sqldriver "database/sql/driver"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	driver "github.com/go-sql-driver/mysql"
	"io"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

type configuration struct {
	DSN, BaseURL, ServiceToken, Installation, Customer, World string
	Mode, Fault                                               string
}

//go:embed migrations/0001_customer_delivery.sql
var customerDDL string
var errUnknown = errors.New("original outcome unresolved")
var errRejected = errors.New("closed customer delivery rejected")

type result struct {
	State                 string
	Persisted, Confirmed  int
	Reconciled, Retried   bool
	OriginalActiveAtQuery bool
}
type customer struct {
	cfg       configuration
	db        *sql.DB
	http      *http.Client
	base      *url.URL
	faultUsed bool
	pending   *sql.Tx
	result    result
}
type original struct {
	notice           generated.IntegrationNotification
	payload, message [32]byte
	key              []any
	lock             string
}

// This example deliberately has only synthetic loopback IO. Secrets enter via
// the bounded stdin object and never become argv, diagnostic text or output.
func openCustomer(c configuration) (*customer, error) {
	u, e := url.Parse(c.BaseURL)
	if e != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errRejected
	}
	for _, id := range []string{c.Installation, c.Customer, c.World} {
		if domain.ValidateID(id) != nil {
			return nil, errRejected
		}
	}
	d, e := driver.ParseDSN(c.DSN)
	if e != nil || d.Net != "tcp" || d.Addr != "127.0.0.1:33267" || strings.EqualFold(d.User, "root") || !regexp.MustCompile(`^loreviva_test_[a-f0-9]{24}$`).MatchString(d.DBName) {
		return nil, errRejected
	}
	d.ParseTime = true
	d.MultiStatements = false
	d.Timeout = 5 * time.Second
	d.ReadTimeout = 5 * time.Second
	d.WriteTimeout = 5 * time.Second
	connector, e := driver.NewConnector(d)
	if e != nil {
		return nil, errRejected
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	x := &customer{cfg: c, db: sql.OpenDB(connector), base: u, http: &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	x.db.SetMaxOpenConns(2)
	return x, nil
}
func (c *customer) close() { c.http.CloseIdleConnections(); _ = c.db.Close() }
func (c *customer) request(ctx context.Context, method, path, key string, body any, schema string, out any) error {
	var b []byte
	if body != nil {
		var e error
		b, e = json.Marshal(body)
		if e != nil {
			return errRejected
		}
	}
	q, e := http.NewRequestWithContext(ctx, method, c.base.String()+path, bytes.NewReader(b))
	if e != nil {
		return errRejected
	}
	q.Header.Set("Authorization", "Bearer "+c.cfg.ServiceToken)
	q.Header.Set("Loreviva-Client-Contract", "integration_r0_v1")
	if body != nil {
		q.Header.Set("Content-Type", "application/json")
		q.Header.Set("Idempotency-Key", key)
	}
	r, e := c.http.Do(q)
	if e != nil {
		return errUnknown
	}
	defer r.Body.Close()
	raw, e := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if e != nil {
		return errUnknown
	}
	if r.StatusCode != 200 && r.StatusCode != 201 {
		return errRejected
	}
	if generated.Decode(schema, raw, out) != nil {
		return errRejected
	}
	return nil
}
func (c *customer) identity(n generated.IntegrationNotification) (original, error) {
	var out original
	if n.Scope.WorldInstanceID != c.cfg.World || n.NotificationRevision != "1" || n.MessageRef != n.NotificationRef || !((n.Kind == "CONTROL_COMMITTED" && n.SafeSummary == "SYNTHETIC_CONTROL_COMMITTED") || (n.Kind == "RESULT_COMMITTED" && n.SafeSummary == "SYNTHETIC_RESULT_COMMITTED")) {
		return out, errRejected
	}
	raw, e := hex.DecodeString(n.PayloadHash)
	if e != nil || len(raw) != 32 || hex.EncodeToString(raw) != n.PayloadHash {
		return out, errRejected
	}
	copy(out.payload[:], raw)
	b, _ := json.Marshal([7]string{c.cfg.Installation, c.cfg.Customer, string(n.ConsumerRef), string(n.NotificationRef), "1", string(n.MessageRef), n.SafeSummary})
	out.message = sha256.Sum256(b)
	out.notice = n
	out.key = []any{c.cfg.Installation, c.cfg.Customer, string(n.ConsumerRef), string(n.NotificationRef), uint64(1)}
	b, _ = json.Marshal([5]string{c.cfg.Installation, c.cfg.Customer, string(n.ConsumerRef), string(n.NotificationRef), "1"})
	h := sha256.Sum256(b)
	out.lock = hex.EncodeToString(h[:])
	return out, nil
}

type rowReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// Both exact five-key rows must exist together. Missing one or an altered pair
// is corruption, never a request to manufacture a replacement message.
func (c *customer) pair(ctx context.Context, q rowReader, o original) (bool, error) {
	var payload, message []byte
	var iid, mid, summary, status string
	var created time.Time
	e1 := q.QueryRowContext(ctx, "SELECT payload_hash,message_id,status FROM customer_inbox WHERE installation_id=? AND customer_id=? AND consumer_id=? AND notification_ref=? AND initial_revision=?", o.key...).Scan(&payload, &iid, &status)
	e2 := q.QueryRowContext(ctx, "SELECT message_id,safe_summary,created_at,message_hash FROM customer_messages WHERE installation_id=? AND customer_id=? AND consumer_id=? AND notification_ref=? AND initial_revision=?", o.key...).Scan(&mid, &summary, &created, &message)
	if errors.Is(e1, sql.ErrNoRows) && errors.Is(e2, sql.ErrNoRows) {
		return false, nil
	}
	if e1 != nil || e2 != nil || !bytes.Equal(payload, o.payload[:]) || !bytes.Equal(message, o.message[:]) || iid != string(o.notice.MessageRef) || mid != iid || summary != o.notice.SafeSummary || status != "COMMITTED" || created.IsZero() {
		return false, errRejected
	}
	return true, nil
}
func (c *customer) acquire(ctx context.Context, o original) (*sql.Conn, string, error) {
	conn, e := c.db.Conn(ctx)
	if e != nil {
		return nil, "", errUnknown
	}
	var uuid, name string
	var held sql.NullInt64
	if e = conn.QueryRowContext(ctx, "SELECT @@server_uuid,DATABASE()").Scan(&uuid, &name); e != nil {
		conn.Close()
		return nil, "", errUnknown
	}
	cfg, _ := driver.ParseDSN(c.cfg.DSN)
	if name != cfg.DBName || uuid == "" {
		conn.Close()
		return nil, "", errRejected
	}
	if e = conn.QueryRowContext(ctx, "SELECT GET_LOCK(?,2)", o.lock).Scan(&held); e != nil || !held.Valid || held.Int64 != 1 {
		conn.Close()
		return nil, "", errUnknown
	}
	return conn, uuid, nil
}
func releaseCustomer(conn *sql.Conn, o original) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var done sql.NullInt64
	_ = conn.QueryRowContext(ctx, "SELECT RELEASE_LOCK(?)", o.lock).Scan(&done)
	_ = conn.Close()
}

func (c *customer) writePair(ctx context.Context, conn *sql.Conn, o original, inject bool) error {
	tx, e := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if e != nil {
		return errUnknown
	}
	retained := false
	defer func() {
		if !retained {
			_ = tx.Rollback()
		}
	}()
	found, e := c.pair(ctx, tx, o)
	if e != nil {
		return e
	}
	if found {
		return tx.Commit()
	}
	var now time.Time
	if e = tx.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&now); e != nil {
		return errUnknown
	}
	args := append(append([]any{}, o.key...), o.payload[:], string(o.notice.MessageRef), "COMMITTED")
	if _, e = tx.ExecContext(ctx, "INSERT INTO customer_inbox VALUES(?,?,?,?,?,?,?,?)", args...); e != nil {
		return errRejected
	}
	args = append(append([]any{}, o.key...), string(o.notice.MessageRef), o.notice.SafeSummary, now, o.message[:])
	if _, e = tx.ExecContext(ctx, "INSERT INTO customer_messages VALUES(?,?,?,?,?,?,?,?,?)", args...); e != nil {
		return errRejected
	}
	// This explicit fault retains the original actual transaction and connection
	// while a different physical connection attempts the original session lock.
	// It tests UNKNOWN; it does not claim an OS crash or a real network outage.
	if inject && c.cfg.Fault == "CUSTOMER_TRANSACTION_STILL_ACTIVE" {
		c.faultUsed = true
		c.pending = tx
		retained = true
		return errUnknown
	}
	// Fault modes are explicit synthetic observations, not claimed process crashes.
	if inject && c.cfg.Fault == "CUSTOMER_BEFORE_COMMIT" {
		c.faultUsed = true
		_ = tx.Rollback()
		return errUnknown
	}
	e = tx.Commit()
	if inject && c.cfg.Fault == "CUSTOMER_COMMIT_ACK_LOST" {
		c.faultUsed = true
		return errUnknown
	}
	if e != nil {
		return errUnknown
	}
	return nil
}

func (c *customer) persist(ctx context.Context, o original) error {
	conn, uuid, e := c.acquire(ctx, o)
	if e != nil {
		return e
	}
	e = c.writePair(ctx, conn, o, !c.faultUsed)
	if e == nil {
		releaseCustomer(conn, o)
		return nil
	}
	if !errors.Is(e, errUnknown) {
		releaseCustomer(conn, o)
		return e
	}
	if c.pending != nil {
		next, _, queryErr := c.acquire(ctx, o)
		if next != nil {
			releaseCustomer(next, o)
		}
		// Prove the original transaction still owns its uncommitted exact pair after
		// the bounded second-connection query; no absence query or retry is allowed.
		found, proofErr := c.pair(ctx, c.pending, o)
		c.result.OriginalActiveAtQuery = queryErr != nil && proofErr == nil && found
		_ = c.pending.Rollback()
		c.pending = nil
		releaseCustomer(conn, o)
		return errUnknown
	}
	// Closing the old connection alone is not termination proof. A fresh actual
	// connection must obtain the same server/session lock before absence is used.
	_ = conn.Raw(func(any) error { return sqldriver.ErrBadConn })
	_ = conn.Close()
	next, same, e := c.acquire(ctx, o)
	if e != nil {
		return errUnknown
	}
	defer releaseCustomer(next, o)
	if same != uuid {
		return errUnknown
	}
	found, e := c.pair(ctx, next, o)
	if e != nil {
		return errUnknown
	}
	c.result.Reconciled = true
	if found {
		return nil
	}
	// Exactly one original-key retry follows proven same-DB termination/absence.
	c.result.Retried = true
	if e = c.writePair(ctx, next, o, false); e != nil {
		return errUnknown
	}
	return nil
}
func (c *customer) checkReceipt(o original, r generated.IntegrationReceipt) bool {
	return r.ConsumerRef == o.notice.ConsumerRef && r.NotificationRef == o.notice.NotificationRef && r.NotificationRevision == "1" && r.CustomerMessageRef == o.notice.MessageRef && r.CustomerMessageHash == hex.EncodeToString(o.message[:]) && r.PayloadHash == o.notice.PayloadHash && r.State == "RECORDED" && r.CustomerPersistenceEvidence == "AUTHENTICATED_CUSTOMER_STATEMENT"
}
func (c *customer) ack(ctx context.Context, o original) error {
	if string(o.notice.Receipt) != "null" {
		var r generated.IntegrationReceipt
		if generated.Decode("IntegrationReceipt", o.notice.Receipt, &r) != nil || !c.checkReceipt(o, r) {
			return errRejected
		}
		return nil
	}
	request := generated.IntegrationReceiptRequest{ChallengeNonce: o.notice.Challenge.Nonce, ChallengeRef: o.notice.Challenge.ChallengeRef, CustomerMessageHash: hex.EncodeToString(o.message[:]), CustomerMessageRef: o.notice.MessageRef, CustomerStatus: "PERSISTED", NotificationRevision: "1", PayloadHash: o.notice.PayloadHash}
	var r generated.IntegrationReceipt
	e := c.request(ctx, http.MethodPost, "/world/v1/customer-notifications/"+string(o.notice.NotificationRef)+"/receipts", o.lock, request, "IntegrationReceipt", &r)
	if e == nil {
		if !c.checkReceipt(o, r) {
			return errRejected
		}
		return nil
	}
	// A failed POST is never repeated. Only the exact original notice can prove
	// the existing receipt. Withdrawal or an unavailable query preserves pending.
	var page generated.IntegrationNotificationPage
	path := "/world/v1/customer-notifications?world_instance_id=" + url.QueryEscape(c.cfg.World) + "&notification_ref=" + url.QueryEscape(string(o.notice.NotificationRef))
	if c.request(ctx, http.MethodGet, path, "", nil, "IntegrationNotificationPage", &page) != nil || len(page.Notifications) != 1 {
		return errUnknown
	}
	if generated.Decode("IntegrationReceipt", page.Notifications[0].Receipt, &r) != nil || !c.checkReceipt(o, r) {
		return errUnknown
	}
	return nil
}
func (c *customer) run(ctx context.Context) error {
	if c.cfg.Mode == "init" {
		var n int
		if c.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE()").Scan(&n) != nil || n != 0 {
			return errRejected
		}
		for _, s := range strings.Split(customerDDL, ";") {
			if strings.TrimSpace(s) != "" {
				if _, e := c.db.ExecContext(ctx, s); e != nil {
					return errRejected
				}
			}
		}
		c.result.State = "INITIALIZED"
		return nil
	}
	if c.cfg.Mode != "" && c.cfg.Mode != "once" {
		return errRejected
	}
	cursor := ""
	for pageNumber := 0; pageNumber < 20; pageNumber++ {
		path := "/world/v1/customer-notifications?world_instance_id=" + url.QueryEscape(c.cfg.World) + "&limit=20"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		var page generated.IntegrationNotificationPage
		if e := c.request(ctx, http.MethodGet, path, "", nil, "IntegrationNotificationPage", &page); e != nil {
			return e
		}
		for _, n := range page.Notifications {
			o, e := c.identity(n)
			if e != nil {
				return e
			}
			if e = c.persist(ctx, o); e != nil {
				return e
			}
			c.result.Persisted++
			if e = c.ack(ctx, o); e != nil {
				return e
			}
			c.result.Confirmed++
		}
		if page.NextCursor == cursor {
			c.result.State = "CONFIRMED"
			return nil
		}
		cursor = page.NextCursor
	}
	c.result.State = "DRAIN_LIMIT"
	return nil
}

func main() {
	var c configuration
	d := json.NewDecoder(io.LimitReader(os.Stdin, 1<<20))
	d.DisallowUnknownFields()
	if d.Decode(&c) != nil || c.DSN == "" || c.ServiceToken == "" || c.Installation == "" || c.Customer == "" || c.World == "" {
		_, _ = os.Stdout.WriteString("{\"state\":\"INVALID_INPUT\"}\n")
		os.Exit(1)
	}
	if d.Decode(new(any)) != io.EOF {
		os.Exit(1)
	}
	if c.Fault != "" && c.Fault != "CUSTOMER_BEFORE_COMMIT" && c.Fault != "CUSTOMER_COMMIT_ACK_LOST" && c.Fault != "CUSTOMER_TRANSACTION_STILL_ACTIVE" {
		os.Exit(1)
	}
	x, e := openCustomer(c)
	if e != nil {
		_, _ = os.Stdout.WriteString("{\"state\":\"INVALID_INPUT\"}\n")
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
	e = x.run(ctx)
	cancel()
	x.close()
	if e != nil {
		x.result.State = "REJECTED"
		if errors.Is(e, errUnknown) {
			x.result.State = "UNKNOWN"
		}
	}
	_ = json.NewEncoder(os.Stdout).Encode(x.result)
	if e != nil {
		os.Exit(1)
	}
}
