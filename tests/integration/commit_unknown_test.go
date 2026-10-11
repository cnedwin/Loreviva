package integration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	store "loreviva.local/loreviva/internal/store/mysql"
	"testing"
	"time"
)

func dispatch(r store.BarrierRequest) store.DispatchRequest {
	r.CommitKey = "authorize"
	r.CommandKey = "dispatch"
	return store.DispatchRequest{BarrierRequest: r, AttemptID: "attempt", AuthorizationID: "authorization", RequestKey: "request", Route: "mock.route", RequestHash: [32]byte{1}, Slot: 0, Reservations: []store.Reservation{{AccountID: "account", Kind: "customer", ScopeKey: "t.p", ReservationID: "reservation", Amount: 7}}}
}

func TestCommitReplyLostQueriesSameReceipt(t *testing.T) {
	s, db, r := fixture(t, true)
	ctx := context.Background()
	r.Locks = []store.LockKey{{Rank: 110, Kind: "events", IDs: []string{"t", "p", "event"}}}
	s.Commit = func(_ context.Context, tx *sql.Tx) error {
		if e := tx.Commit(); e != nil {
			return e
		}
		return errors.New("injected: durable COMMIT reply lost")
	}
	_, e := s.WithBarrier(ctx, r, func(tx store.Tx) error {
		if e := tx.RequireLock(store.LockKey{Rank: 110, Kind: "events", IDs: []string{"t", "p", "event"}}); e != nil {
			return e
		}
		_, e := tx.ExecContext(ctx, "INSERT INTO events(tenant_id,product_id,record_id,task_id,owner_subject_id,status,revision) VALUES('t','p','event','task','owner','COMMITTED',1)")
		return e
	})
	if !errors.Is(e, store.ErrCommitUnknown) {
		t.Fatalf("expected local COMMIT UNKNOWN, got %v", e)
	}
	s.Commit = nil
	receipt, e := s.QueryCommitScoped(ctx, r.ProductScope, r.WorldScope, r.OwnerSubjectID, r.CommitKey)
	if e != nil || !receipt.Committed {
		t.Fatal("original receipt unavailable", e)
	}
	_, e = s.WithBarrier(ctx, r, func(store.Tx) error { t.Fatal("unknown commit replay reran effect"); return nil })
	if e != nil {
		t.Fatal(e)
	}
	var count int
	scanAssertion(t, db.QueryRow("SELECT COUNT(*) FROM events"), &count)
	if count != 1 {
		t.Fatalf("events=%d", count)
	}
	t.Log("durable reply loss: original receipt recovered, events=1")
}

func TestAuthorizedWithoutSendMarkCannotCallNetwork(t *testing.T) {
	s, db, r := fixture(t, true)
	ctx := context.Background()
	d := dispatch(r)
	a, e := s.AuthorizeDispatch(ctx, d)
	if e != nil {
		t.Fatal(e)
	}
	if a.CanSend {
		t.Fatal("AUTHORIZED grants network without marker")
	}
	var started sql.NullTime
	var state string
	scanAssertion(t, db.QueryRow("SELECT state,send_started_at FROM attempts"), &state, &started)
	if state != "AUTHORIZED" || started.Valid {
		t.Fatal("authorization wrote send marker", state, started)
	}
	d.CommitKey = "send"
	a, e = s.StartSend(ctx, d, d.AttemptID)
	if e != nil || !a.CanSend || !a.Receipt.Committed {
		t.Fatal("known marker receipt required", a, e)
	}
	a, e = s.StartSend(ctx, d, d.AttemptID)
	if e != nil || a.CanSend || !a.QueryOriginal {
		t.Fatal("repeat send reauthorized", a, e)
	}
	var held, slots int
	scanAssertion(t, db.QueryRow("SELECT held FROM budget_accounts"), &held)
	scanAssertion(t, db.QueryRow("SELECT calls_used FROM tasks"), &slots)
	if held != 7 || slots != 1 {
		t.Fatal("duplicate reservation or slot", held, slots)
	}
}

func TestSendStartCrashNeverRepurchases(t *testing.T) {
	for _, window := range []string{"before-marker-commit", "marker-commit-reply-lost", "committed-before-network", "after-network"} {
		t.Run(window, func(t *testing.T) {
			s, db, r := fixture(t, true)
			ctx := context.Background()
			d := dispatch(r)
			if _, e := s.AuthorizeDispatch(ctx, d); e != nil {
				t.Fatal(e)
			}
			d.CommitKey = "send"
			purchases := 0
			switch window {
			case "before-marker-commit":
				s.BeforeCommit = func(context.Context, *sql.Tx) error { return errors.New("crash before COMMIT") }
			case "marker-commit-reply-lost":
				s.Commit = func(_ context.Context, tx *sql.Tx) error {
					if e := tx.Commit(); e != nil {
						return e
					}
					return errors.New("lost reply")
				}
			}
			a, e := s.StartSend(ctx, d, d.AttemptID)
			if window == "before-marker-commit" {
				if e == nil || a.CanSend {
					t.Fatal("precommit crash authorized send")
				}
			} else if window == "marker-commit-reply-lost" {
				if !errors.Is(e, store.ErrCommitUnknown) || a.CanSend {
					t.Fatal("ambiguous marker authorized send", e)
				}
			} else if e != nil || !a.CanSend {
				t.Fatal(e, a)
			}
			if window == "after-network" {
				if !a.CanSend {
					t.Fatal("purchase without receipt")
				}
				purchases++
			}
			s.BeforeCommit = nil
			s.Commit = nil
			if _, e = db.Exec("UPDATE tasks SET lease_expires_at=?", time.Now().UTC().Add(-time.Hour)); e != nil {
				t.Fatal(e)
			}
			lease, e := s.ClaimTask(ctx, "takeover", time.Now().UTC(), time.Hour)
			if e != nil {
				t.Fatal(e)
			}
			d.WorkerID = "takeover"
			d.Fence = lease.Fence
			a, e = s.StartSend(ctx, d, d.AttemptID)
			if e != nil {
				t.Fatal(e)
			}
			if window == "before-marker-commit" {
				if !a.CanSend {
					t.Fatal("confirmed rollback cannot safely retry marker", a)
				}
				purchases++
			} else {
				if a.CanSend || !a.QueryOriginal {
					t.Fatal("takeover repurchases SENT", a)
				}
				if _, e = s.QueryCommitScoped(ctx, r.ProductScope, r.WorldScope, r.OwnerSubjectID, "send"); e != nil {
					t.Fatal(e)
				}
			}
			if purchases > 1 {
				t.Fatal("duplicate purchase")
			}
			var slots, held, attempts int
			scanAssertion(t, db.QueryRow("SELECT calls_used FROM tasks"), &slots)
			scanAssertion(t, db.QueryRow("SELECT held FROM budget_accounts"), &held)
			scanAssertion(t, db.QueryRow("SELECT COUNT(*) FROM attempts"), &attempts)
			if slots != 1 || held != 7 || attempts != 1 {
				t.Fatal("takeover reset durable counters", slots, held, attempts)
			}
			t.Logf("%s purchases=%d slots=%d held=%d attempts=%d", window, purchases, slots, held, attempts)
		})
	}
}

func TestGateChangesDenyDispatchAndCommit(t *testing.T) {
	for _, gate := range []string{"tenant", "product", "subject", "control", "privacy", "cancel", "lease", "release"} {
		t.Run(gate, func(t *testing.T) {
			s, db, r := fixture(t, true)
			ctx := context.Background()
			d := dispatch(r)
			q := map[string]string{"tenant": "UPDATE tenant_gates SET enabled=0", "product": "UPDATE product_gates SET enabled=0", "subject": "UPDATE subject_governance SET permission_version=2", "control": "UPDATE world_controls SET status='PAUSED',control_version=2", "privacy": "UPDATE subject_governance SET privacy_epoch=2", "cancel": "UPDATE tasks SET cancelled=1", "lease": "UPDATE tasks SET lease_expires_at='2000-01-01'", "release": "UPDATE releases SET available=0"}[gate]
			if _, e := db.Exec(q); e != nil {
				t.Fatal(e)
			}
			if _, e := s.AuthorizeDispatch(ctx, d); e == nil {
				t.Fatal("gate change allowed dispatch")
			}
			if _, e := s.WithBarrier(ctx, r, func(store.Tx) error { return nil }); e == nil {
				t.Fatal("gate change allowed commit")
			}
			var count int
			scanAssertion(t, db.QueryRow("SELECT COUNT(*) FROM attempts"), &count)
			if count != 0 {
				t.Fatal(fmt.Sprint("denied dispatch wrote attempt", count))
			}
		})
	}
}

func TestScopedReceiptDatabaseFailureIsNotPermissionDenial(t *testing.T) {
	s, _, r := fixture(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, e := s.QueryCommitScoped(ctx, r.ProductScope, nil, r.OwnerSubjectID, r.CommitKey)
	if !errors.Is(e, context.Canceled) {
		t.Fatalf("database query unavailable became permission denial: %v", e)
	}
}
