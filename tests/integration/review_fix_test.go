package integration

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	store "loreviva.local/loreviva/internal/store/mysql"
)

func scanAssertion(t *testing.T, row *sql.Row, destinations ...any) {
	t.Helper()
	if e := row.Scan(destinations...); e != nil {
		t.Fatal("database assertion read failed:", e)
	}
}

func extraDispatch(d store.DispatchRequest) store.DispatchRequest {
	d.CommitKey, d.CommandKey = "extra-authorize", "extra-command"
	d.AttemptID, d.AuthorizationID, d.RequestKey = "extra-attempt", "extra-authorization", "extra-request"
	d.Route, d.RequestHash, d.Slot = "extra.route", [32]byte{2}, 1
	d.Reservations = append([]store.Reservation(nil), d.Reservations...)
	d.Reservations[0].ReservationID = "extra-reservation"
	return d
}

func TestUnresolvedAttemptDeniesFreshSlotAfterTakeover(t *testing.T) {
	for _, state := range []string{"AUTHORIZED", "SENT", "UNKNOWN"} {
		t.Run(state, func(t *testing.T) {
			s, db, r := fixture(t, true)
			ctx := context.Background()
			d := dispatch(r)
			if _, e := s.AuthorizeDispatch(ctx, d); e != nil {
				t.Fatal(e)
			}
			if state != "AUTHORIZED" {
				d.CommitKey = "send"
				a, e := s.StartSend(ctx, d, d.AttemptID)
				if e != nil || !a.CanSend {
					t.Fatal(a, e)
				}
				if _, e = db.Exec("UPDATE attempts SET state=?", state); e != nil {
					t.Fatal(e)
				}
			}
			if _, e := db.Exec("UPDATE tasks SET lease_expires_at=?", time.Now().UTC().Add(-time.Hour)); e != nil {
				t.Fatal(e)
			}
			lease, e := s.ClaimTask(ctx, "takeover", time.Now().UTC(), time.Hour)
			if e != nil || lease.Fence != 2 {
				t.Fatal(lease, e)
			}
			d.WorkerID, d.Fence = "takeover", lease.Fence
			extra := extraDispatch(d)
			a, e := s.AuthorizeDispatch(ctx, extra)
			if !errors.Is(e, store.ErrDenied) || a.CanSend {
				t.Errorf("fresh extra authorized: %+v %v", a, e)
			}
			extra.CommitKey = "extra-send"
			a, e = s.StartSend(ctx, extra, extra.AttemptID)
			if !errors.Is(e, store.ErrDenied) || a.CanSend {
				t.Errorf("fresh extra send boundary: %+v %v", a, e)
			}
			var attempts, authorizations, reservations, held, calls, receipts, extraMarkers int
			scanAssertion(t, db.QueryRow("SELECT COUNT(*) FROM attempts"), &attempts)
			scanAssertion(t, db.QueryRow("SELECT COUNT(*) FROM dispatch_authorizations"), &authorizations)
			scanAssertion(t, db.QueryRow("SELECT COUNT(*) FROM reservations"), &reservations)
			scanAssertion(t, db.QueryRow("SELECT held FROM budget_accounts"), &held)
			scanAssertion(t, db.QueryRow("SELECT calls_used FROM tasks"), &calls)
			scanAssertion(t, db.QueryRow("SELECT COUNT(*) FROM commit_receipts WHERE commit_key IN ('extra-authorize','extra-send')"), &receipts)
			scanAssertion(t, db.QueryRow("SELECT COUNT(*) FROM lock_markers WHERE lock_key LIKE '%extra%'"), &extraMarkers)
			if attempts != 1 || authorizations != 1 || reservations != 1 || held != 7 || calls != 1 || receipts != 0 || extraMarkers != 0 {
				t.Errorf("extra durable effects: %d %d %d %d %d %d %d", attempts, authorizations, reservations, held, calls, receipts, extraMarkers)
			}
			t.Logf("%s after fence=%d: attempts=%d calls=%d held=%d extra receipts=%d", state, lease.Fence, attempts, calls, held, receipts)
			d.CommitKey = "send"
			a, e = s.StartSend(ctx, d, d.AttemptID)
			if e != nil {
				t.Fatal("original-slot recovery failed", e)
			}
			if state == "AUTHORIZED" {
				if !a.CanSend {
					t.Fatal("confirmed unsent original cannot retry marker", a)
				}
			} else if a.CanSend || !a.QueryOriginal {
				t.Fatal("unresolved original repurchased", a)
			}
		})
	}
}

func TestSeededExtraAttemptCannotStartSend(t *testing.T) {
	s, db, r := fixture(t, true)
	ctx := context.Background()
	d := dispatch(r)
	if _, e := s.AuthorizeDispatch(ctx, d); e != nil {
		t.Fatal(e)
	}
	d = extraDispatch(d)
	// Synthetic legacy/corrupt fixture bypasses admission to exercise the final boundary.
	for _, q := range []string{
		"INSERT INTO attempts(tenant_id,product_id,task_id,attempt_id,slot,request_key,route,request_hash,state) VALUES('t','p','task','extra-attempt',1,'extra-request','extra.route',UNHEX(CONCAT('02',REPEAT('00',31))),'AUTHORIZED')",
		"INSERT INTO dispatch_authorizations(tenant_id,product_id,task_id,attempt_id,authorization_id,fence,owner_subject_id) VALUES('t','p','task','extra-attempt','extra-authorization',1,'owner')",
		"INSERT INTO reservations(tenant_id,product_id,task_id,kind,scope_key,account_id,reservation_id,purpose,amount,status) VALUES('t','p','task','customer','t.p','account','extra-reservation','extra-attempt',7,'HELD')",
		"UPDATE tasks SET calls_used=2",
		"UPDATE budget_accounts SET held=14",
	} {
		if _, e := db.Exec(q); e != nil {
			t.Fatal(e)
		}
	}
	d.CommitKey = "extra-send"
	a, e := s.StartSend(ctx, d, d.AttemptID)
	if !errors.Is(e, store.ErrDenied) || a.CanSend {
		t.Errorf("seeded extra acquired send permission: %+v %v", a, e)
	}
	var marker sql.NullTime
	var state string
	var receipts, held, calls int
	scanAssertion(t, db.QueryRow("SELECT state,send_started_at FROM attempts WHERE attempt_id='extra-attempt'"), &state, &marker)
	scanAssertion(t, db.QueryRow("SELECT COUNT(*) FROM commit_receipts WHERE commit_key='extra-send'"), &receipts)
	scanAssertion(t, db.QueryRow("SELECT held FROM budget_accounts"), &held)
	scanAssertion(t, db.QueryRow("SELECT calls_used FROM tasks"), &calls)
	if marker.Valid || state != "AUTHORIZED" || receipts != 0 || held != 14 || calls != 2 {
		t.Fatal(state, marker, receipts, held, calls)
	}
}

func TestAttemptSchemaCanonicalVocabulary(t *testing.T) {
	_, db, _ := fixture(t, true)
	for _, state := range []string{"RESERVED", "AUTHORIZED", "SENT", "PENDING", "UNKNOWN", "SUCCEEDED", "FAILED", "CANCEL_CONFIRMED"} {
		t.Run(state, func(t *testing.T) {
			var started, marker any
			if state != "RESERVED" && state != "AUTHORIZED" {
				started, marker = time.Now().UTC(), "send"
			}
			if _, e := db.Exec("INSERT INTO attempts(tenant_id,product_id,task_id,attempt_id,slot,request_key,route,request_hash,state,send_started_at,marker_commit_key) VALUES('t','p','task',?,0,?,'mock.route',UNHEX(REPEAT('00',32)),?,?,?)", state, state, state, started, marker); e != nil {
				t.Fatal(e)
			}
			var actual string
			scanAssertion(t, db.QueryRow("SELECT state FROM attempts WHERE attempt_id=?", state), &actual)
			if actual != state {
				t.Fatal(actual)
			}
			if _, e := db.Exec("DELETE FROM attempts"); e != nil {
				t.Fatal(e)
			}
		})
	}
	if _, e := db.Exec("INSERT INTO attempts(tenant_id,product_id,task_id,attempt_id,slot,request_key,route,request_hash,state,send_started_at,marker_commit_key) VALUES('t','p','task','received',0,'received','mock.route',UNHEX(REPEAT('00',32)),'RECEIVED',UTC_TIMESTAMP(6),'send')"); e == nil {
		t.Fatal("unmapped RECEIVED accepted")
	}
	for _, state := range []string{"FAILED", "CANCEL_CONFIRMED"} {
		t.Run("unsent-"+state, func(t *testing.T) {
			if _, e := db.Exec("INSERT INTO attempts(tenant_id,product_id,task_id,attempt_id,slot,request_key,route,request_hash,state) VALUES('t','p','task',?,0,?,'mock.route',UNHEX(REPEAT('00',32)),?)", state, state, state); e != nil {
				t.Fatal(e)
			}
			if _, e := db.Exec("DELETE FROM attempts"); e != nil {
				t.Fatal(e)
			}
		})
	}
	for _, state := range []string{"SENT", "PENDING", "UNKNOWN", "SUCCEEDED"} {
		t.Run("missing-marker-"+state, func(t *testing.T) {
			if _, e := db.Exec("INSERT INTO attempts(tenant_id,product_id,task_id,attempt_id,slot,request_key,route,request_hash,state) VALUES('t','p','task',?,0,?,'mock.route',UNHEX(REPEAT('00',32)),?)", state, state, state); e == nil {
				t.Fatal("outbound state without marker accepted")
			}
		})
	}
}
