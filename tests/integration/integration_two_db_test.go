package integration

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	driver "github.com/go-sql-driver/mysql"
	"io"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/localstate"
	"loreviva.local/loreviva/tests/integration/dbfixture"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Test-only transport collection retains newly returned nonce/cursor values in
// RAM until the existing private scan channel receives them. Unwrap preserves
// the real transport write-deadline capability.
type customerScanWriter struct {
	http.ResponseWriter
	collect func([]byte)
}

func (w *customerScanWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *customerScanWriter) Write(b []byte) (int, error) {
	w.collect(b)
	return w.ResponseWriter.Write(b)
}

func TestIntegrationJointDeliveryAndReceipt(t *testing.T) {
	f, _ := deliveryFragmentFixture(t, false)
	page, e := f.r.Delivery.GetNotifications(f.service, f.world.WorldInstanceID, "", "", 20)
	if e != nil || len(page.Notifications) != 2 {
		t.Fatal("actual two initial joints", e)
	}
	retainSessionScanMaterial(t, page.NextCursor)
	for _, n := range page.Notifications {
		retainSessionScanMaterial(t, n.Challenge.Nonce)
	}
	db := dbfixture.Open(t)
	raw, e := os.ReadFile("../../examples/thin-backend/migrations/0001_customer_delivery.sql")
	if e != nil {
		t.Fatal("fixed customer DDL prerequisite", e)
	}
	for _, statement := range strings.Split(string(raw), ";") {
		if strings.TrimSpace(statement) != "" {
			if _, e = db.Exec(statement); e != nil {
				t.Fatal("customer two-table DDL", e)
			}
		}
	}
	var dbname, installation, customer string
	if e = db.QueryRow("SELECT DATABASE()").Scan(&dbname); e != nil {
		t.Fatal(e)
	}
	if e = f.r.Store.DB.QueryRow("SELECT installation_id FROM memory_installation WHERE singleton=1").Scan(&installation); e != nil {
		t.Fatal(e)
	}
	if e = f.r.Store.DB.QueryRow("SELECT customer_id FROM integration_notifications WHERE notice_ref=?", page.Notifications[0].NotificationRef).Scan(&customer); e != nil {
		t.Fatal(e)
	}
	cfg, e := driver.ParseDSN(os.Getenv("LOREVIVA_TEST_DSN"))
	if e != nil {
		t.Fatal("private customer connector")
	}
	cfg.DBName = dbname
	cfg.ParseTime = true
	retainSessionScanMaterial(t, cfg.FormatDSN())
	var collectedMu sync.Mutex
	var collected []string
	collect := func(b []byte) {
		var page generated.IntegrationNotificationPage
		if generated.Decode("IntegrationNotificationPage", b, &page) == nil {
			collectedMu.Lock()
			defer collectedMu.Unlock()
			collected = append(collected, page.NextCursor)
			for _, n := range page.Notifications {
				collected = append(collected, n.Challenge.Nonce)
			}
		}
	}
	defer func() {
		collectedMu.Lock()
		defer collectedMu.Unlock()
		for _, v := range collected {
			if v != "" {
				retainSessionScanMaterial(t, v)
			}
		}
	}()
	var premature atomic.Bool
	var posts atomic.Int32
	handler := f.r.Handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/receipts") {
			parts := strings.Split(r.URL.Path, "/")
			id := parts[len(parts)-2]
			var n int
			e := db.QueryRow("SELECT COUNT(*) FROM customer_inbox i JOIN customer_messages m USING(installation_id,customer_id,consumer_id,notification_ref,initial_revision) WHERE i.notification_ref=? AND i.status='COMMITTED' AND i.message_id=m.message_id", id).Scan(&n)
			if e != nil || n != 1 {
				premature.Store(true)
				w.WriteHeader(500)
				return
			}
			posts.Add(1)
		}
		handler.ServeHTTP(&customerScanWriter{ResponseWriter: w, collect: collect}, r)
	}))
	defer srv.Close()
	input, _ := json.Marshal(map[string]string{"DSN": cfg.FormatDSN(), "BaseURL": srv.URL, "ServiceToken": "synthetic-s66-create-service-secret-32", "Installation": installation, "Customer": customer, "World": f.world.WorldInstanceID})
	exe := filepath.Join(t.TempDir(), "thin-backend.exe")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	build := exec.CommandContext(ctx, os.Getenv("LOREVIVA_TEST_GO_EXE"), "build", "-o", exe, "./examples/thin-backend")
	build.Dir = "../.."
	if output, e := build.CombinedOutput(); e != nil {
		t.Fatal("customer build prerequisite (not business RED)", e, string(output))
	}
	command := exec.CommandContext(ctx, exe)
	command.Stdin = bytes.NewReader(input)
	var output, stderr bytes.Buffer
	command.Stdout = &output
	command.Stderr = &stderr
	e = command.Run()
	var result struct {
		State                string
		Persisted, Confirmed int
	}
	if json.Unmarshal(output.Bytes(), &result) != nil {
		t.Fatal("customer safe result malformed")
	}
	if e != nil || result.State != "CONFIRMED" || result.Persisted != 2 || result.Confirmed != 2 || premature.Load() || posts.Load() != 2 {
		t.Fatal("causal customer owning port after lawful joints and actual independent two tables", e, result.State, result.Persisted, result.Confirmed, premature.Load(), posts.Load())
	}
	var n int
	if e = db.QueryRow("SELECT COUNT(*) FROM customer_messages").Scan(&n); e != nil || n != 2 {
		t.Fatal("actual customer message count", n, e)
	}
}

func TestIntegrationNotificationChallengeAndCurrent(t *testing.T) {
	t.Run("real-MEDIA-hidden-progress-and-EOF", func(t *testing.T) {
		f, _ := deliveryFragmentFixture(t, true)
		before, e := f.r.Delivery.GetNotifications(f.service, f.world.WorldInstanceID, "", "", 20)
		if e != nil || len(before.Notifications) != 2 {
			t.Fatal("original two joints", e)
		}
		retainSessionScanMaterial(t, before.NextCursor)
		for _, n := range before.Notifications {
			retainSessionScanMaterial(t, n.Challenge.Nonce)
		}
		decode, _ := notificationCursorCodec(t, f)
		old := decode(before.NextCursor)
		if worked, e := f.r.Delivery.ProduceOneSyntheticMedia(context.Background()); e != nil || !worked {
			t.Fatal("real media producer", worked, e)
		}
		after, e := f.r.Delivery.GetNotifications(f.service, f.world.WorldInstanceID, "", before.NextCursor, 20)
		if e != nil || len(after.Notifications) != 0 || after.NextCursor == before.NextCursor {
			t.Fatal("hidden MEDIA must advance without inventing a notice", e)
		}
		retainSessionScanMaterial(t, after.NextCursor)
		next := decode(after.NextCursor)
		if next["Issued"] != old["Issued"] || next["Expires"] != old["Expires"] || next["Position"] == old["Position"] {
			t.Fatal("hidden progress or original expiry")
		}
		eof, e := f.r.Delivery.GetNotifications(f.service, f.world.WorldInstanceID, "", after.NextCursor, 20)
		if e != nil || len(eof.Notifications) != 0 || eof.NextCursor != after.NextCursor {
			t.Fatal("zero-progress EOF preserves bytes", e)
		}
	})
	t.Run("CONTROL-does-not-invent-TURN-use", func(t *testing.T) {
		f := integrationFixSetup(t, true)
		if _, e := f.r.Store.ApplyPlayerSelfConsent(f.client, "notice-control-revoke", json.RawMessage(`{"command":"REVOKE","expected_revision":"1","policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`)); e != nil {
			t.Fatal("real REVOKE prerequisite", e)
		}
		page, e := f.r.Delivery.GetNotifications(f.service, f.world.WorldInstanceID, "", "", 20)
		if e != nil || len(page.Notifications) != 1 || page.Notifications[0].Kind != "CONTROL_COMMITTED" {
			t.Fatal("CONTROL original safe metadata does not borrow TURN positive use", e)
		}
		retainSessionScanMaterial(t, page.NextCursor)
		retainSessionScanMaterial(t, page.Notifications[0].Challenge.Nonce)
	})
	f, _ := deliveryFragmentFixture(t, false)
	var notices, pairs int
	if e := f.r.Store.DB.QueryRow("SELECT COUNT(*),SUM(o.record_id IS NOT NULL) FROM integration_notifications n JOIN outbox o ON o.tenant_id=n.tenant_id AND o.product_id=n.product_id AND o.record_id=n.outbox_ref WHERE n.world_instance_id=?", f.world.WorldInstanceID).Scan(&notices, &pairs); e != nil || notices != 2 || pairs != 2 {
		t.Fatal("actual CONTROL and TURN initial joint prerequisites", notices, pairs, e)
	}
	port, ok := any(f.r.Delivery).(interface {
		GetNotifications(context.Context, string, string, string, uint16) (generated.IntegrationNotificationPage, error)
	})
	if !ok {
		t.Fatal("CAUSAL_RED: real registered SERVICE and two owning joint notices reached missing notification port")
	}
	page, e := port.GetNotifications(f.service, f.world.WorldInstanceID, "", "", 20)
	if e != nil || len(page.Notifications) != 2 {
		t.Fatal("actual original notice page", e, len(page.Notifications))
	}
	for _, n := range page.Notifications {
		retainSessionScanMaterial(t, n.Challenge.Nonce)
	}
	retainSessionScanMaterial(t, page.NextCursor)
	first := page.Notifications[0]
	t.Run("valid-nonce-does-not-renew", func(t *testing.T) {
		one, e := port.GetNotifications(f.service, f.world.WorldInstanceID, string(first.NotificationRef), "", 1)
		if e != nil || len(one.Notifications) != 1 || one.Notifications[0].Challenge != first.Challenge {
			t.Fatal("unchanged valid challenge", e)
		}
		retainSessionScanMaterial(t, one.NextCursor)
		retainSessionScanMaterial(t, one.Notifications[0].Challenge.Nonce)
	})
	t.Run("original-pair-current-and-final", func(t *testing.T) {
		for _, query := range []string{
			"UPDATE integration_notifications SET message_ref='foreign' WHERE notice_ref=?",
			"UPDATE integration_notifications SET payload_hash=UNHEX(REPEAT('00',32)) WHERE notice_ref=?",
			"UPDATE integration_notifications SET visibility_source_refs=JSON_ARRAY('foreign') WHERE notice_ref=?",
		} {
			reached := false
			f.r.Store.BeforeCommit = func(ctx context.Context, tx *sql.Tx) error {
				reached = true
				r, e := tx.ExecContext(ctx, query, first.NotificationRef)
				if e != nil {
					return e
				}
				n, e := r.RowsAffected()
				if e != nil || n != 1 {
					t.Fatal("fault prerequisite", n, e)
				}
				return nil
			}
			_, e := port.GetNotifications(f.service, f.world.WorldInstanceID, "", "", 20)
			f.r.Store.BeforeCommit = nil
			if e == nil || !reached {
				t.Fatal("no output on original-pair final drift", e, reached)
			}
		}
	})
	t.Run("cursor-domain-and-arguments", func(t *testing.T) {
		for _, v := range []string{"not-a-cursor", page.NextCursor + "A"} {
			if _, e := port.GetNotifications(f.service, f.world.WorldInstanceID, "", v, 20); e == nil {
				t.Fatal("bad cursor accepted")
			}
		}
		if _, e := port.GetNotifications(f.service, f.world.WorldInstanceID, string(first.NotificationRef), page.NextCursor, 20); e == nil {
			t.Fatal("notice and cursor are exclusive")
		}
		if _, e := port.GetNotifications(f.client, f.world.WorldInstanceID, "", "", 20); e == nil {
			t.Fatal("CLIENT not SERVICE")
		}
		if _, e := port.GetNotifications(f.service, f.world.WorldInstanceID, "", "", 101); e == nil {
			t.Fatal("limit overflow")
		}
	})
	t.Run("future-append-after-true-EOF", func(t *testing.T) {
		eof, e := port.GetNotifications(f.service, f.world.WorldInstanceID, "", page.NextCursor, 20)
		if e != nil || len(eof.Notifications) != 0 || eof.NextCursor != page.NextCursor {
			t.Fatal("true EOF original bytes", e)
		}
		var cv uint64
		if e = f.r.Store.DB.QueryRow("SELECT control_version FROM world_controls WHERE world_instance_id=?", f.world.WorldInstanceID).Scan(&cv); e != nil {
			t.Fatal(e)
		}
		f.control(t, f.client, "PAUSE", strconv.FormatUint(cv, 10), "notice-late-pause")
		next, e := port.GetNotifications(f.service, f.world.WorldInstanceID, "", eof.NextCursor, 1)
		if e != nil || len(next.Notifications) != 1 || next.NextCursor == eof.NextCursor {
			t.Fatal("append after old H must be visible", e)
		}
		retainSessionScanMaterial(t, next.NextCursor)
		retainSessionScanMaterial(t, next.Notifications[0].Challenge.Nonce)
	})
	t.Run("nonce-natural-expiry-and-rotation", func(t *testing.T) {
		expires, e := time.Parse(time.RFC3339Nano, first.Challenge.ExpiresAt)
		if e != nil {
			t.Fatal(e)
		}
		var now time.Time
		if e = f.r.Store.DB.QueryRow("SELECT UTC_TIMESTAMP(6)").Scan(&now); e != nil {
			t.Fatal(e)
		}
		remaining := expires.Sub(now)
		if remaining <= 0 || remaining > 120*time.Second {
			t.Fatal("PRECONDITION_FAIL actual nonce window", remaining)
		}
		t.Logf("actual nonce remaining %.3f seconds; waiting once without changing expiry", remaining.Seconds())
		time.Sleep(remaining + 50*time.Millisecond)
		var installation, customer string
		if e = f.r.Store.DB.QueryRow("SELECT installation_id FROM memory_installation WHERE singleton=1").Scan(&installation); e != nil {
			t.Fatal(e)
		}
		if e = f.r.Store.DB.QueryRow("SELECT customer_id FROM integration_notifications WHERE notice_ref=?", first.NotificationRef).Scan(&customer); e != nil {
			t.Fatal(e)
		}
		b, _ := json.Marshal([7]string{installation, customer, string(first.ConsumerRef), string(first.NotificationRef), "1", string(first.MessageRef), first.SafeSummary})
		h := sha256.Sum256(b)
		input := generated.IntegrationReceiptRequest{ChallengeNonce: first.Challenge.Nonce, ChallengeRef: first.Challenge.ChallengeRef, CustomerMessageHash: hex.EncodeToString(h[:]), CustomerMessageRef: first.MessageRef, CustomerStatus: "PERSISTED", NotificationRevision: "1", PayloadHash: first.PayloadHash}
		if _, e = f.r.Delivery.RecordReceipt(f.service, string(first.NotificationRef), "expired-nonce", input); e == nil {
			t.Fatal("expired nonce accepted")
		}
		one, e := port.GetNotifications(f.service, f.world.WorldInstanceID, string(first.NotificationRef), "", 1)
		if e != nil || len(one.Notifications) != 1 {
			t.Fatal("lawful GET rotation", e)
		}
		fresh := one.Notifications[0].Challenge
		retainSessionScanMaterial(t, one.NextCursor)
		retainSessionScanMaterial(t, fresh.Nonce)
		if fresh.Nonce == first.Challenge.Nonce || fresh.ChallengeRef == first.Challenge.ChallengeRef {
			t.Fatal("expired challenge must rotate")
		}
		if _, e = f.r.Delivery.RecordReceipt(f.service, string(first.NotificationRef), "expired-nonce", input); e == nil {
			t.Fatal("old nonce remains rejected after rotation")
		}
		input.ChallengeNonce = fresh.Nonce
		input.ChallengeRef = fresh.ChallengeRef
		if _, e = f.r.Delivery.RecordReceipt(f.service, string(first.NotificationRef), "expired-nonce", input); e != nil {
			t.Fatal("same business after lawful rotation", e)
		}
	})
	t.Run("ordered-hundred-and-closed-cursor", func(t *testing.T) {
		var cv uint64
		var status string
		if e = f.r.Store.DB.QueryRow("SELECT control_version,status FROM world_controls WHERE world_instance_id=?", f.world.WorldInstanceID).Scan(&cv, &status); e != nil {
			t.Fatal(e)
		}
		for i := 0; i < 100; i++ {
			action := "PAUSE"
			if status == "PAUSED" {
				action = "RESUME"
			}
			f.control(t, f.client, action, strconv.FormatUint(cv, 10), "notice-order-"+strconv.Itoa(i))
			cv++
			if action == "PAUSE" {
				status = "PAUSED"
			} else {
				status = "RUNNING"
			}
		}
		seen := map[generated.Identity]bool{}
		cursor := ""
		for i := 0; i < 20; i++ {
			next, e := port.GetNotifications(f.service, f.world.WorldInstanceID, "", cursor, 20)
			if e != nil {
				t.Fatal("actual ordered producer page", e)
			}
			retainSessionScanMaterial(t, next.NextCursor)
			for _, n := range next.Notifications {
				retainSessionScanMaterial(t, n.Challenge.Nonce)
				if seen[n.NotificationRef] {
					t.Fatal("duplicate initial notice")
				}
				seen[n.NotificationRef] = true
			}
			if next.NextCursor == cursor {
				break
			}
			cursor = next.NextCursor
		}
		var total int
		if e = f.r.Store.DB.QueryRow("SELECT COUNT(*) FROM integration_notifications WHERE world_instance_id=?", f.world.WorldInstanceID).Scan(&total); e != nil || len(seen) != total || total < 102 {
			t.Fatal("all real initial notices traversed", len(seen), total, e)
		}
		full, e := port.GetNotifications(f.service, f.world.WorldInstanceID, "", "", 100)
		if e != nil || len(full.Notifications) != 100 {
			t.Fatal("bounded limit100", len(full.Notifications), e)
		}
		retainSessionScanMaterial(t, full.NextCursor)
		for _, n := range full.Notifications {
			retainSessionScanMaterial(t, n.Challenge.Nonce)
		}
		decode, encode := notificationCursorCodec(t, f)
		valid := encode(decode(full.NextCursor))
		if _, e := port.GetNotifications(f.service, f.world.WorldInstanceID, "", valid, 20); e != nil {
			t.Fatal("fixture codec must reach valid authenticated production binding", e)
		}
		for _, test := range []struct {
			name   string
			mutate func(map[string]any)
		}{
			{"missing-position", func(v map[string]any) { delete(v, "Position") }},
			{"unknown-field", func(v map[string]any) { v["Unexpected"] = true }},
			{"foreign-consumer", func(v map[string]any) { v["Binding"].(map[string]any)["Consumer"] = "foreign" }},
			{"generation", func(v map[string]any) { v["Binding"].(map[string]any)["Generation"] = float64(0) }},
			{"position-after-H", func(v map[string]any) { v["Position"] = v["HighWater"].(float64) + 1 }},
			{"expiry-renewed", func(v map[string]any) { v["Expires"] = time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano) }},
		} {
			t.Run(test.name, func(t *testing.T) {
				v := decode(full.NextCursor)
				test.mutate(v)
				bad := encode(v)
				if _, e := port.GetNotifications(f.service, f.world.WorldInstanceID, "", bad, 20); e == nil {
					t.Fatal("authenticated malformed cursor accepted")
				}
			})
		}
	})
}

// This codec is confined to the owned fixture's real CONFIG key. It creates
// authenticated malformed DATA for negative tests, never a production permit.
func notificationCursorCodec(t *testing.T, f integrationFixFixture) (func(string) map[string]any, func(map[string]any) string) {
	t.Helper()
	const tag = "loreviva/NOTIFICATION_V1/1"
	root, e := localstate.Open(filepath.Dir(f.r.Local.Coordinator.Directory()), false)
	if e != nil {
		t.Fatal(e)
	}
	_, id, done, e := f.r.Local.Participant.EnterConfiguration(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	material, e := root.SessionHMACKey(id.Installation, false)
	done()
	if e != nil {
		t.Fatal(e)
	}
	h := hmac.New(sha256.New, material[:])
	h.Write([]byte(tag))
	block, e := aes.NewCipher(h.Sum(nil))
	if e != nil {
		t.Fatal(e)
	}
	gcm, e := cipher.NewGCM(block)
	if e != nil {
		t.Fatal(e)
	}
	decode := func(token string) map[string]any {
		b, e := base64.RawURLEncoding.DecodeString(token)
		if e != nil || len(b) < gcm.NonceSize() {
			t.Fatal("owned cursor encoding")
		}
		raw, e := gcm.Open(nil, b[:gcm.NonceSize()], b[gcm.NonceSize():], []byte(tag))
		if e != nil {
			t.Fatal("owned cursor authentication")
		}
		var v map[string]any
		if json.Unmarshal(raw, &v) != nil {
			t.Fatal("owned cursor data")
		}
		return v
	}
	encode := func(v map[string]any) string {
		// Preserve the actual producer member order so negative cases reach the
		// selected binding check rather than a different serialization guard.
		ordered := func(obj map[string]any, keys []string) []byte {
			seen := map[string]bool{}
			parts := []string{}
			for _, k := range keys {
				seen[k] = true
				if x, ok := obj[k]; ok {
					kb, _ := json.Marshal(k)
					b, _ := json.Marshal(x)
					parts = append(parts, string(kb)+":"+string(b))
				}
			}
			extra := []string{}
			for k := range obj {
				if !seen[k] {
					extra = append(extra, k)
				}
			}
			sort.Strings(extra)
			for _, k := range extra {
				kb, _ := json.Marshal(k)
				b, _ := json.Marshal(obj[k])
				parts = append(parts, string(kb)+":"+string(b))
			}
			return []byte("{" + strings.Join(parts, ",") + "}")
		}
		copy := map[string]any{}
		for k, x := range v {
			copy[k] = x
		}
		if binding, ok := v["Binding"].(map[string]any); ok {
			copy["Binding"] = json.RawMessage(ordered(binding, []string{"Installation", "Tenant", "Product", "Customer", "Consumer", "World", "Service", "Generation", "WorldPrivacy", "WorldView", "SubjectPrivacy", "SubjectView", "Consent", "CustomerRevision", "MappingRevision", "EntitlementRevision", "KeyVersion"}))
		}
		raw := ordered(copy, []string{"Version", "Binding", "Position", "HighWater", "Issued", "Expires"})
		n := make([]byte, gcm.NonceSize())
		if _, e := rand.Read(n); e != nil {
			t.Fatal(e)
		}
		token := base64.RawURLEncoding.EncodeToString(gcm.Seal(n, n, raw, []byte(tag)))
		retainSessionScanMaterial(t, token)
		return token
	}
	return decode, encode
}

func TestIntegrationNotificationReceiptIdempotency(t *testing.T) {
	f, _ := deliveryFragmentFixture(t, false)
	page, e := f.r.Delivery.GetNotifications(f.service, f.world.WorldInstanceID, "", "", 20)
	if e != nil || len(page.Notifications) != 2 {
		t.Fatal("owning two notices prerequisite", e)
	}
	retainSessionScanMaterial(t, page.NextCursor)
	for _, n := range page.Notifications {
		retainSessionScanMaterial(t, n.Challenge.Nonce)
	}
	var installation string
	if e = f.r.Store.DB.QueryRow("SELECT installation_id FROM memory_installation WHERE singleton=1").Scan(&installation); e != nil {
		t.Fatal(e)
	}
	port, ok := any(f.r.Delivery).(interface {
		RecordReceipt(context.Context, string, string, generated.IntegrationReceiptRequest) (generated.IntegrationReceipt, error)
	})
	for _, n := range page.Notifications {
		retainSessionScanMaterial(t, n.Challenge.Nonce)
		if !ok {
			t.Fatal("CAUSAL_RED: lawful original CONTROL/TURN notice and nonce reached missing owning receipt port")
		}
		var customer string
		if e = f.r.Store.DB.QueryRow("SELECT customer_id FROM integration_notifications WHERE notice_ref=?", n.NotificationRef).Scan(&customer); e != nil {
			t.Fatal(e)
		}
		raw, _ := json.Marshal([7]string{installation, customer, string(n.ConsumerRef), string(n.NotificationRef), string(n.NotificationRevision), string(n.MessageRef), n.SafeSummary})
		h := sha256.Sum256(raw)
		input := generated.IntegrationReceiptRequest{ChallengeNonce: n.Challenge.Nonce, ChallengeRef: n.Challenge.ChallengeRef, CustomerMessageHash: hex.EncodeToString(h[:]), CustomerMessageRef: n.MessageRef, CustomerStatus: "PERSISTED", NotificationRevision: n.NotificationRevision, PayloadHash: n.PayloadHash}
		if n.Kind == "CONTROL_COMMITTED" {
			for _, test := range []struct {
				name   string
				mutate func(*generated.IntegrationReceiptRequest)
			}{
				{"wrong-nonce", func(v *generated.IntegrationReceiptRequest) { v.ChallengeNonce = strings.Repeat("A", 43) }},
				{"wrong-challenge", func(v *generated.IntegrationReceiptRequest) { v.ChallengeRef = "foreign" }},
				{"wrong-message", func(v *generated.IntegrationReceiptRequest) { v.CustomerMessageRef = "foreign" }},
				{"wrong-customer-hash", func(v *generated.IntegrationReceiptRequest) { v.CustomerMessageHash = strings.Repeat("0", 64) }},
				{"wrong-payload-hash", func(v *generated.IntegrationReceiptRequest) { v.PayloadHash = strings.Repeat("0", 64) }},
				{"wrong-revision", func(v *generated.IntegrationReceiptRequest) { v.NotificationRevision = "2" }},
			} {
				t.Run(test.name, func(t *testing.T) {
					v := input
					test.mutate(&v)
					if _, e := port.RecordReceipt(f.service, string(n.NotificationRef), "bad-"+test.name, v); e == nil {
						t.Fatal("invalid original tuple accepted")
					}
				})
			}
			for _, test := range []struct{ name, query string }{
				{"receipt-owner", "UPDATE integration_receipts SET owner_id='foreign' WHERE operation='recordCustomerNotificationReceipt'"},
				{"receipt-operation", "UPDATE integration_receipts SET operation='foreign' WHERE operation='recordCustomerNotificationReceipt'"},
				{"receipt-scope", "UPDATE integration_receipts SET scope_address='foreign' WHERE operation='recordCustomerNotificationReceipt'"},
				{"receipt-hash", "UPDATE integration_receipts SET payload_hash=UNHEX(REPEAT('00',32)) WHERE operation='recordCustomerNotificationReceipt'"},
				{"receipt-revision", "UPDATE integration_receipts SET target_revision=2 WHERE operation='recordCustomerNotificationReceipt'"},
				{"receipt-metadata", "UPDATE integration_receipts SET result_metadata=JSON_SET(result_metadata,'$.customer_message_ref','foreign') WHERE operation='recordCustomerNotificationReceipt'"},
				{"outbox-tuple", "UPDATE outbox SET message_id='foreign' WHERE status='ACKED'"},
			} {
				t.Run("final-"+test.name, func(t *testing.T) {
					reached := false
					f.r.Store.BeforeCommit = func(ctx context.Context, tx *sql.Tx) error {
						reached = true
						r, e := tx.ExecContext(ctx, test.query)
						if e != nil {
							return e
						}
						k, e := r.RowsAffected()
						if e != nil || k != 1 {
							t.Fatal("fault prerequisite did not change exactly one row", k, e)
						}
						return nil
					}
					_, e := port.RecordReceipt(f.service, string(n.NotificationRef), "final-"+test.name, input)
					f.r.Store.BeforeCommit = nil
					if e == nil || !reached {
						t.Fatal("fresh full tuple final must reject", e, reached)
					}
					var count int
					if e = f.r.Store.DB.QueryRow("SELECT COUNT(*) FROM integration_receipts WHERE operation='recordCustomerNotificationReceipt'").Scan(&count); e != nil || count != 0 {
						t.Fatal("receipt rollback", count, e)
					}
					if e = f.r.Store.DB.QueryRow("SELECT COUNT(*) FROM outbox WHERE status='ACKED'").Scan(&count); e != nil || count != 0 {
						t.Fatal("outbox rollback", count, e)
					}
				})
			}
		}
		a, e := port.RecordReceipt(f.service, string(n.NotificationRef), "receipt-"+n.Kind, input)
		if e != nil {
			t.Fatal("original receipt", e)
		}
		b, e := port.RecordReceipt(f.service, string(n.NotificationRef), "receipt-"+n.Kind, input)
		if e != nil || a != b {
			t.Fatal("exact replay", e)
		}
		t.Run("original-"+n.Kind, func(t *testing.T) {
			if _, e := port.RecordReceipt(f.client, string(n.NotificationRef), "receipt-"+n.Kind, input); e == nil {
				t.Fatal("CLIENT cannot use SERVICE ACK")
			}
			if _, e := port.RecordReceipt(f.service, string(n.NotificationRef), "another-key", input); e == nil {
				t.Fatal("notice must not gain a second receipt")
			}
			changed := input
			changed.CustomerMessageHash = strings.Repeat("0", 64)
			if _, e := port.RecordReceipt(f.service, string(n.NotificationRef), "receipt-"+n.Kind, changed); e == nil {
				t.Fatal("same key different business hash")
			}
			one, e := f.r.Delivery.GetNotifications(f.service, f.world.WorldInstanceID, string(n.NotificationRef), "", 1)
			if e != nil || len(one.Notifications) != 1 {
				t.Fatal("exact receipt query", e)
			}
			retainSessionScanMaterial(t, one.NextCursor)
			retainSessionScanMaterial(t, one.Notifications[0].Challenge.Nonce)
			var original generated.IntegrationReceipt
			if json.Unmarshal(one.Notifications[0].Receipt, &original) != nil || original != a {
				t.Fatal("query original receipt identity")
			}
		})
	}
	// This is an actual transport prerequisite, not a recorder with no deadline.
	t.Run("SERVICE-HTTP-GET-and-POST", func(t *testing.T) {
		srv := httptest.NewServer(f.r.Handler())
		defer srv.Close()
		q, _ := http.NewRequest(http.MethodGet, srv.URL+"/world/v1/customer-notifications?world_instance_id="+f.world.WorldInstanceID, nil)
		q.Header.Set("Authorization", "Bearer synthetic-s66-create-service-secret-32")
		resp, e := srv.Client().Do(q)
		if e != nil {
			t.Fatal(e)
		}
		defer resp.Body.Close()
		body, e := io.ReadAll(resp.Body)
		var got generated.IntegrationNotificationPage
		if e != nil || resp.StatusCode != 200 || generated.Decode("IntegrationNotificationPage", body, &got) != nil {
			t.Fatal("causal HTTP route after owning ACK", resp.StatusCode, e)
		}
		retainSessionScanMaterial(t, got.NextCursor)
		for _, n := range got.Notifications {
			retainSessionScanMaterial(t, n.Challenge.Nonce)
			var customer string
			if e = f.r.Store.DB.QueryRow("SELECT customer_id FROM integration_notifications WHERE notice_ref=?", n.NotificationRef).Scan(&customer); e != nil {
				t.Fatal(e)
			}
			b, _ := json.Marshal([7]string{installation, customer, string(n.ConsumerRef), string(n.NotificationRef), "1", string(n.MessageRef), n.SafeSummary})
			h := sha256.Sum256(b)
			input := generated.IntegrationReceiptRequest{ChallengeNonce: n.Challenge.Nonce, ChallengeRef: n.Challenge.ChallengeRef, CustomerMessageHash: hex.EncodeToString(h[:]), CustomerMessageRef: n.MessageRef, CustomerStatus: "PERSISTED", NotificationRevision: "1", PayloadHash: n.PayloadHash}
			body, _ := json.Marshal(input)
			post, _ := http.NewRequest(http.MethodPost, srv.URL+"/world/v1/customer-notifications/"+string(n.NotificationRef)+"/receipts", strings.NewReader(string(body)))
			post.Header.Set("Authorization", "Bearer synthetic-s66-create-service-secret-32")
			post.Header.Set("Content-Type", "application/json")
			post.Header.Set("Idempotency-Key", "receipt-"+n.Kind)
			ack, e := srv.Client().Do(post)
			if e != nil {
				t.Fatal(e)
			}
			raw, e := io.ReadAll(ack.Body)
			_ = ack.Body.Close()
			if e != nil || ack.StatusCode != 201 || generated.Validate("IntegrationReceipt", raw) != nil || ack.Header.Get("Cache-Control") != "no-store" {
				t.Fatal("actual SERVICE HTTP ACK", ack.StatusCode, e)
			}
		}
	})
}

// The child is a real independent customer process. These hooks belong only to
// the owning test transport; they never supply platform receipts or authority.
type customerHarness struct {
	f          integrationFixFixture
	db         *sql.DB
	cfg        map[string]string
	exe        string
	posts      atomic.Int32
	beforePost func(*http.Request)
	afterPost  func(*http.Request)
	loseACK    bool
}
type customerSafeResult struct {
	State                                      string
	Persisted, Confirmed                       int
	Reconciled, Retried, OriginalActiveAtQuery bool
}
type customerLostReplyWriter struct {
	http.ResponseWriter
	status int
}

func (w *customerLostReplyWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *customerLostReplyWriter) WriteHeader(n int)           { w.status = n }
func (w *customerLostReplyWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	return len(b), nil
}
func buildCustomerExample(t *testing.T) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "customer.exe")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, os.Getenv("LOREVIVA_TEST_GO_EXE"), "build", "-o", exe, "./examples/thin-backend")
	c.Dir = "../.."
	if output, e := c.CombinedOutput(); e != nil {
		t.Fatal("customer compile prerequisite", e, string(output))
	}
	return exe
}
func newCustomerHarness(t *testing.T, exe string, init bool) *customerHarness {
	t.Helper()
	f, _ := deliveryFragmentFixture(t, false)
	db := dbfixture.Open(t)
	if init {
		raw, e := os.ReadFile("../../examples/thin-backend/migrations/0001_customer_delivery.sql")
		if e != nil {
			t.Fatal(e)
		}
		for _, s := range strings.Split(string(raw), ";") {
			if strings.TrimSpace(s) != "" {
				if _, e = db.Exec(s); e != nil {
					t.Fatal(e)
				}
			}
		}
	}
	var name, installation, customer string
	if e := db.QueryRow("SELECT DATABASE()").Scan(&name); e != nil {
		t.Fatal(e)
	}
	if e := f.r.Store.DB.QueryRow("SELECT installation_id FROM memory_installation WHERE singleton=1").Scan(&installation); e != nil {
		t.Fatal(e)
	}
	if e := f.r.Store.DB.QueryRow("SELECT customer_id FROM integration_notifications LIMIT 1").Scan(&customer); e != nil {
		t.Fatal(e)
	}
	d, e := driver.ParseDSN(os.Getenv("LOREVIVA_TEST_DSN"))
	if e != nil {
		t.Fatal("customer DSN prerequisite")
	}
	d.DBName = name
	d.ParseTime = true
	retainSessionScanMaterial(t, d.FormatDSN())
	h := &customerHarness{f: f, db: db, exe: exe, cfg: map[string]string{"DSN": d.FormatDSN(), "ServiceToken": "synthetic-s66-create-service-secret-32", "Installation": installation, "Customer": customer, "World": f.world.WorldInstanceID}}
	var mu sync.Mutex
	var secrets []string
	collect := func(b []byte) {
		var p generated.IntegrationNotificationPage
		if generated.Decode("IntegrationNotificationPage", b, &p) == nil {
			mu.Lock()
			defer mu.Unlock()
			secrets = append(secrets, p.NextCursor)
			for _, n := range p.Notifications {
				secrets = append(secrets, n.Challenge.Nonce)
			}
		}
	}
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		for _, s := range secrets {
			if s != "" {
				retainSessionScanMaterial(t, s)
			}
		}
	})
	handler := f.r.Handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			h.posts.Add(1)
			if h.beforePost != nil {
				h.beforePost(r)
			}
			if h.loseACK {
				lost := &customerLostReplyWriter{ResponseWriter: w}
				handler.ServeHTTP(lost, r)
				if h.afterPost != nil {
					h.afterPost(r)
				}
				conn, _, e := http.NewResponseController(w).Hijack()
				if e == nil {
					_ = conn.Close()
				}
				return
			}
		}
		handler.ServeHTTP(&customerScanWriter{ResponseWriter: w, collect: collect}, r)
	}))
	t.Cleanup(srv.Close)
	h.cfg["BaseURL"] = srv.URL
	return h
}
func (h *customerHarness) run(t *testing.T, mode, fault string) (customerSafeResult, error) {
	t.Helper()
	v := map[string]string{}
	for k, s := range h.cfg {
		v[k] = s
	}
	v["Mode"] = mode
	v["Fault"] = fault
	raw, _ := json.Marshal(v)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.exe)
	cmd.Stdin = bytes.NewReader(raw)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	e := cmd.Run()
	var r customerSafeResult
	if json.Unmarshal(out.Bytes(), &r) != nil {
		t.Fatal("child safe result missing", e)
	}
	if stderr.Len() != 0 {
		_, _ = os.Stderr.Write(stderr.Bytes())
		t.Fatal("unexpected child diagnostic output captured for scan-before-read")
	}
	return r, e
}
func (h *customerHarness) counts(t *testing.T) (int, int, int) {
	t.Helper()
	var a, b, c int
	if e := h.db.QueryRow("SELECT COUNT(*) FROM customer_inbox").Scan(&a); e != nil {
		t.Fatal(e)
	}
	if e := h.db.QueryRow("SELECT COUNT(*) FROM customer_messages").Scan(&b); e != nil {
		t.Fatal(e)
	}
	if e := h.f.r.Store.DB.QueryRow("SELECT COUNT(*) FROM integration_receipts WHERE operation='recordCustomerNotificationReceipt'").Scan(&c); e != nil {
		t.Fatal(e)
	}
	return a, b, c
}
func TestIntegrationCustomerCommitUnknown(t *testing.T) {
	exe := buildCustomerExample(t)
	for _, fault := range []string{"CUSTOMER_BEFORE_COMMIT", "CUSTOMER_COMMIT_ACK_LOST", "CUSTOMER_TRANSACTION_STILL_ACTIVE"} {
		t.Run(fault, func(t *testing.T) {
			h := newCustomerHarness(t, exe, true)
			r, e := h.run(t, "once", fault)
			a, b, c := h.counts(t)
			if fault == "CUSTOMER_TRANSACTION_STILL_ACTIVE" {
				if e == nil || r.State != "UNKNOWN" || !r.OriginalActiveAtQuery || r.Retried || r.Reconciled || a != 0 || b != 0 || c != 0 || h.posts.Load() != 0 {
					t.Fatal("original active transaction must remain UNKNOWN with no retry/ACK", r, e, a, b, c, h.posts.Load())
				}
				return
			}
			if e != nil || r.State != "CONFIRMED" || !r.Reconciled || r.Retried != (fault == "CUSTOMER_BEFORE_COMMIT") || a != 2 || b != 2 || c != 2 || h.posts.Load() != 2 {
				t.Fatal("actual original outcome query and bounded retry", r, e, a, b, c, h.posts.Load())
			}
		})
	}
	t.Run("partial-pair-is-not-absence", func(t *testing.T) {
		h := newCustomerHarness(t, exe, true)
		r, e := h.run(t, "once", "")
		if e != nil || r.State != "CONFIRMED" {
			t.Fatal("lawful pair control", r, e)
		}
		var id string
		if e = h.db.QueryRow("SELECT notification_ref FROM customer_messages ORDER BY notification_ref LIMIT 1").Scan(&id); e != nil {
			t.Fatal(e)
		}
		res, e := h.db.Exec("DELETE FROM customer_messages WHERE notification_ref=?", id)
		if e != nil {
			t.Fatal(e)
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			t.Fatal("fault prerequisite", n)
		}
		before := h.posts.Load()
		r, e = h.run(t, "once", "")
		a, b, c := h.counts(t)
		if e == nil || r.State != "REJECTED" || a != 2 || b != 1 || c != 2 || h.posts.Load() != before {
			t.Fatal("partial pair must never be repaired or re-ACKed", r, e, a, b, c)
		}
	})
}
func TestIntegrationPlatformReceiptUnknownAndRevocation(t *testing.T) {
	exe := buildCustomerExample(t)
	for _, kind := range []string{"lost-ACK", "revoke-before-TURN-ACK", "lost-ACK-then-revoke"} {
		t.Run(kind, func(t *testing.T) {
			h := newCustomerHarness(t, exe, true)
			h.loseACK = kind != "revoke-before-TURN-ACK"
			var once sync.Once
			revoke := func(r *http.Request) {
				parts := strings.Split(r.URL.Path, "/")
				id := parts[len(parts)-2]
				var k string
				if e := h.f.r.Store.DB.QueryRow("SELECT kind FROM integration_notifications WHERE notice_ref=?", id).Scan(&k); e != nil {
					t.Error("notice lookup", e)
					return
				}
				if k == "RESULT_COMMITTED" {
					once.Do(func() {
						_, e := h.f.r.Store.ApplyPlayerSelfConsent(h.f.client, "customer-real-revoke", json.RawMessage(`{"command":"REVOKE","expected_revision":"1","policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`))
						if e != nil {
							t.Error("real self REVOKE prerequisite", e)
						}
					})
				}
			}
			if kind == "revoke-before-TURN-ACK" {
				h.beforePost = revoke
			}
			if kind == "lost-ACK-then-revoke" {
				h.afterPost = revoke
			}
			r, e := h.run(t, "once", "")
			a, b, c := h.counts(t)
			if kind == "lost-ACK" {
				if e != nil || r.State != "CONFIRMED" || a != 2 || b != 2 || c != 2 || h.posts.Load() != 2 {
					t.Fatal("lost reply must query original actual receipts", r, e, a, b, c, h.posts.Load())
				}
				return
			}
			expected := 2
			if kind == "revoke-before-TURN-ACK" {
				expected = 1
			}
			if e == nil || r.State != "UNKNOWN" || a != 2 || b != 2 || c != expected || h.posts.Load() != 2 {
				t.Fatal("revoked query cannot infer absence or retry; customer rows retained", r, e, a, b, c, h.posts.Load())
			}
		})
	}
}
func TestIntegrationCustomerDDLAndClose(t *testing.T) {
	exe := buildCustomerExample(t)
	t.Run("fresh-exact-two-table-and-existing-reject", func(t *testing.T) {
		h := newCustomerHarness(t, exe, false)
		r, e := h.run(t, "init", "")
		if e != nil || r.State != "INITIALIZED" {
			t.Fatal("actual initializer", r, e)
		}
		var tables, keys, checks int
		if e = h.db.QueryRow("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE()").Scan(&tables); e != nil {
			t.Fatal(e)
		}
		if e = h.db.QueryRow("SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema=DATABASE() AND index_name='PRIMARY'").Scan(&keys); e != nil {
			t.Fatal(e)
		}
		if e = h.db.QueryRow("SELECT COUNT(*) FROM information_schema.table_constraints WHERE table_schema=DATABASE() AND constraint_type='CHECK'").Scan(&checks); e != nil {
			t.Fatal(e)
		}
		if tables != 2 || keys != 10 || checks != 4 {
			t.Fatal("exact customer structures", tables, keys, checks)
		}
		r, e = h.run(t, "init", "")
		if e == nil || r.State != "REJECTED" {
			t.Fatal("existing init rejected", r, e)
		}
		r, e = h.run(t, "once", "")
		if e != nil || r.State != "CONFIRMED" {
			t.Fatal("initialized owning customer use", r, e)
		}
		var id string
		if e = h.db.QueryRow("SELECT notification_ref FROM customer_messages LIMIT 1").Scan(&id); e != nil {
			t.Fatal(e)
		}
		if _, e = h.db.Exec("UPDATE customer_inbox SET status='PENDING' WHERE notification_ref=?", id); e == nil {
			t.Fatal("real COMMITTED CHECK did not reject")
		}
		if _, e = h.db.Exec("UPDATE customer_messages SET safe_summary='private-body' WHERE notification_ref=?", id); e == nil {
			t.Fatal("real summary CHECK did not reject")
		}
		if _, e = h.db.Exec("INSERT INTO customer_messages SELECT * FROM customer_messages WHERE notification_ref=?", id); e == nil {
			t.Fatal("real five-key duplicate did not reject")
		}
		r, e = h.run(t, "once", "")
		a, b, c := h.counts(t)
		if e != nil || r.State != "CONFIRMED" || a != 2 || b != 2 || c != 2 || h.posts.Load() != 2 {
			t.Fatal("new customer process queries originals, no duplicate ACK", r, e, a, b, c)
		}
	})
	t.Run("partial-init-refusal-preserves-existing", func(t *testing.T) {
		h := newCustomerHarness(t, exe, false)
		if _, e := h.db.Exec("CREATE TABLE customer_inbox (original_marker INT NOT NULL)"); e != nil {
			t.Fatal(e)
		}
		if _, e := h.db.Exec("INSERT INTO customer_inbox VALUES(7)"); e != nil {
			t.Fatal(e)
		}
		r, e := h.run(t, "init", "")
		var n, marker int
		if e2 := h.db.QueryRow("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE()").Scan(&n); e2 != nil {
			t.Fatal(e2)
		}
		if e2 := h.db.QueryRow("SELECT original_marker FROM customer_inbox").Scan(&marker); e2 != nil {
			t.Fatal(e2)
		}
		if e == nil || r.State != "REJECTED" || n != 1 || marker != 7 || h.posts.Load() != 0 {
			t.Fatal("no implicit partial initialization/repair", r, e, n, marker)
		}
	})
}

// MEDIA remains invisible, but its initial TURN and existing notification pair
// must still be proved even when the incoming cursor has passed that TURN.
func TestIntegrationHiddenMediaOriginalPair(t *testing.T) {
	f, task := deliveryFragmentFixture(t, true)
	before, e := f.r.Delivery.GetNotifications(f.service, f.world.WorldInstanceID, "", "", 20)
	if e != nil || len(before.Notifications) != 2 {
		t.Fatal("lawful original two notices", e)
	}
	retainSessionScanMaterial(t, before.NextCursor)
	var turn generated.IntegrationNotification
	for _, n := range before.Notifications {
		retainSessionScanMaterial(t, n.Challenge.Nonce)
		if n.Kind == "RESULT_COMMITTED" {
			turn = n
		}
	}
	if turn.NotificationRef == "" {
		t.Fatal("real TURN notice missing")
	}
	if worked, e := f.r.Delivery.ProduceOneSyntheticMedia(context.Background()); e != nil || !worked {
		t.Fatal("real MEDIA append", worked, e)
	}
	read := func(t *testing.T, want bool) {
		t.Helper()
		page, e := f.r.Delivery.GetNotifications(f.service, f.world.WorldInstanceID, "", before.NextCursor, 20)
		if page.NextCursor != "" {
			retainSessionScanMaterial(t, page.NextCursor)
		}
		for _, n := range page.Notifications {
			retainSessionScanMaterial(t, n.Challenge.Nonce)
		}
		if want {
			if e != nil || len(page.Notifications) != 0 || page.NextCursor == "" || page.NextCursor == before.NextCursor {
				t.Fatal("lawful hidden MEDIA progress", e)
			}
		} else if e == nil || page.NextCursor != "" || len(page.Notifications) != 0 {
			t.Error("CAUSAL_RED: invalid original joint advanced hidden MEDIA")
		}
	}
	read(t, true)
	var initial, outbox string
	if e = f.r.Store.DB.QueryRow("SELECT business_update_id FROM delivery_updates WHERE world_instance_id=? AND task_ref=? AND kind='TURN'", f.world.WorldInstanceID, task).Scan(&initial); e != nil {
		t.Fatal(e)
	}
	if e = f.r.Store.DB.QueryRow("SELECT outbox_ref FROM integration_notifications WHERE notice_ref=?", turn.NotificationRef).Scan(&outbox); e != nil {
		t.Fatal(e)
	}
	type fault struct {
		name, query, restore string
		args, restoreArgs    []any
	}
	faults := []fault{
		{"initial-task", "UPDATE delivery_updates SET task_ref='foreign' WHERE business_update_id=?", "UPDATE delivery_updates SET task_ref=? WHERE business_update_id=?", []any{initial}, []any{task, initial}},
		{"notice-message", "UPDATE integration_notifications SET message_ref='foreign' WHERE notice_ref=?", "UPDATE integration_notifications SET message_ref=? WHERE notice_ref=?", []any{turn.NotificationRef}, []any{turn.MessageRef, turn.NotificationRef}},
		{"outbox-message", "UPDATE outbox SET message_id='foreign' WHERE record_id=?", "UPDATE outbox SET message_id=? WHERE record_id=?", []any{outbox}, []any{turn.MessageRef, outbox}},
	}
	counts := func() [4]uint64 {
		t.Helper()
		var c [4]uint64
		if e := f.r.Store.DB.QueryRow("SELECT (SELECT MAX(update_seq) FROM delivery_updates WHERE world_instance_id=?),(SELECT COUNT(*) FROM integration_notifications),(SELECT COUNT(*) FROM integration_challenges),(SELECT COUNT(*) FROM integration_receipts WHERE operation='recordCustomerNotificationReceipt')", f.world.WorldInstanceID).Scan(&c[0], &c[1], &c[2], &c[3]); e != nil {
			t.Fatal(e)
		}
		return c
	}
	for _, x := range faults {
		t.Run(x.name, func(t *testing.T) {
			base := counts()
			r, e := f.r.Store.DB.Exec(x.query, x.args...)
			if e != nil {
				t.Fatal("fault prerequisite", e)
			}
			n, e := r.RowsAffected()
			if e != nil || n != 1 {
				t.Fatal("fault row", n, e)
			}
			defer func() {
				if _, e := f.r.Store.DB.Exec(x.restore, x.restoreArgs...); e != nil {
					t.Error("restore", e)
				}
			}()
			read(t, false)
			if counts() != base {
				t.Fatal("rejected read wrote H/notice/challenge/receipt")
			}
		})
	}
	t.Run("final-pair-drift", func(t *testing.T) {
		base := counts()
		reached := false
		f.r.Store.BeforeCommit = func(ctx context.Context, tx *sql.Tx) error {
			reached = true
			r, e := tx.ExecContext(ctx, faults[2].query, faults[2].args...)
			if e != nil {
				return e
			}
			n, e := r.RowsAffected()
			if e != nil || n != 1 {
				return fmt.Errorf("fault row %d: %v", n, e)
			}
			return nil
		}
		read(t, false)
		f.r.Store.BeforeCommit = nil
		var actual string
		if e := f.r.Store.DB.QueryRow("SELECT message_id FROM outbox WHERE record_id=?", outbox).Scan(&actual); e != nil {
			t.Fatal(e)
		}
		if actual != string(turn.MessageRef) {
			if _, e := f.r.Store.DB.Exec(faults[2].restore, faults[2].restoreArgs...); e != nil {
				t.Fatal("restore after RED", e)
			}
			t.Error("final drift committed instead of rollback")
		}
		if !reached || counts() != base {
			t.Fatal("final reach/no new writes", reached)
		}
	})
	read(t, true)
	var installation, customer string
	if e = f.r.Store.DB.QueryRow("SELECT installation_id FROM memory_installation WHERE singleton=1").Scan(&installation); e != nil {
		t.Fatal(e)
	}
	if e = f.r.Store.DB.QueryRow("SELECT customer_id FROM integration_notifications WHERE notice_ref=?", turn.NotificationRef).Scan(&customer); e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal([7]string{installation, customer, string(turn.ConsumerRef), string(turn.NotificationRef), string(turn.NotificationRevision), string(turn.MessageRef), turn.SafeSummary})
	h := sha256.Sum256(raw)
	input := generated.IntegrationReceiptRequest{ChallengeNonce: turn.Challenge.Nonce, ChallengeRef: turn.Challenge.ChallengeRef, CustomerMessageHash: hex.EncodeToString(h[:]), CustomerMessageRef: turn.MessageRef, CustomerStatus: "PERSISTED", NotificationRevision: turn.NotificationRevision, PayloadHash: turn.PayloadHash}
	if _, e = f.r.Delivery.RecordReceipt(f.service, string(turn.NotificationRef), "hidden-media-real-ack", input); e != nil {
		t.Fatal("real ACKED prerequisite", e)
	}
	t.Run("ACKED-pair-positive", func(t *testing.T) {
		base := counts()
		read(t, true)
		if counts() != base {
			t.Fatal("hidden ACKED read wrote new record")
		}
	})
	t.Run("existing-ACK-identity", func(t *testing.T) {
		r, e := f.r.Store.DB.Exec("UPDATE integration_receipts SET target_revision=2 WHERE original_key=? AND operation='recordCustomerNotificationReceipt'", "hidden-media-real-ack")
		if e != nil {
			t.Fatal("ACK fault prerequisite", e)
		}
		n, e := r.RowsAffected()
		if e != nil || n != 1 {
			t.Fatal("ACK fault row", n, e)
		}
		defer func() {
			if _, e := f.r.Store.DB.Exec("UPDATE integration_receipts SET target_revision=1 WHERE original_key=? AND operation='recordCustomerNotificationReceipt'", "hidden-media-real-ack"); e != nil {
				t.Error("restore", e)
			}
		}()
		base := counts()
		read(t, false)
		if counts() != base {
			t.Fatal("invalid ACK hidden write")
		}
	})
}
