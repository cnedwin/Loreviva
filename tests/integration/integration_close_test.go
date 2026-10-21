package integration

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"loreviva.local/loreviva/internal/files"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/localstate"
	"loreviva.local/loreviva/internal/maintenance"
	store "loreviva.local/loreviva/internal/store/mysql"
)

// Closing is tested at real owning SQL/native/transport boundaries. An admitted
// transaction may still be rejected by its final current check; transport loss
// after COMMIT does not turn the durable receipt into an absent operation.
func TestIntegrationCloseActualWriters(t *testing.T) {
	for _, kind := range []string{"challenge", "receipt"} {
		t.Run(kind+"-SQL-drain", func(t *testing.T) {
			f, _ := deliveryFragmentFixture(t, false)
			var notices []string
			rows, e := f.r.Store.DB.Query("SELECT notice_ref FROM integration_notifications ORDER BY notice_ref")
			if e != nil {
				t.Fatal(e)
			}
			for rows.Next() {
				var n string
				if e = rows.Scan(&n); e != nil {
					rows.Close()
					t.Fatal(e)
				}
				notices = append(notices, n)
			}
			if e = rows.Err(); e != nil {
				rows.Close()
				t.Fatal(e)
			}
			if e = rows.Close(); e != nil || len(notices) != 2 {
				t.Fatal("original two joints", e)
			}
			first, e := f.r.Delivery.GetNotifications(f.service, f.world.WorldInstanceID, notices[0], "", 1)
			if e != nil || len(first.Notifications) != 1 {
				t.Fatal("positive challenge", e)
			}
			closeRetainPage(t, first)
			var request generated.IntegrationReceiptRequest
			if kind == "receipt" {
				positive := closeReceiptInput(t, f, first.Notifications[0])
				if _, e = f.r.Delivery.RecordReceipt(f.service, notices[0], "close-positive-ack", positive); e != nil {
					t.Fatal("positive original ACK", e)
				}
				second, e := f.r.Delivery.GetNotifications(f.service, f.world.WorldInstanceID, notices[1], "", 1)
				if e != nil || len(second.Notifications) != 1 {
					t.Fatal(e)
				}
				closeRetainPage(t, second)
				request = closeReceiptInput(t, f, second.Notifications[0])
			}
			before := closeNotificationCounts(t, f)
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			resume := func() { once.Do(func() { close(release) }) }
			defer resume()
			var nonce []byte
			f.r.Store.BeforeCommit = func(ctx context.Context, tx *sql.Tx) error {
				if kind == "challenge" {
					if err := tx.QueryRowContext(ctx, "SELECT nonce FROM integration_challenges WHERE notice_ref=?", notices[1]).Scan(&nonce); err != nil {
						return err
					}
				}
				close(entered)
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			finished := make(chan error, 1)
			go func() {
				if kind == "challenge" {
					_, e := f.r.Delivery.GetNotifications(f.service, f.world.WorldInstanceID, notices[1], "", 1)
					finished <- e
				} else {
					_, e := f.r.Delivery.RecordReceipt(f.service, notices[1], "close-held-ack", request)
					finished <- e
				}
			}()
			closeAwait(t, entered, "actual BeforeCommit")
			if len(nonce) != 0 {
				retainSessionScanMaterial(t, hex.EncodeToString(nonce))
			}
			plan := "close-" + kind
			if e = f.r.Local.Coordinator.BeginClose(context.Background(), plan); e != nil {
				t.Fatal(e)
			}
			if _, e = f.r.Delivery.GetNotifications(f.service, f.world.WorldInstanceID, notices[1], "", 1); !errors.Is(e, maintenance.ErrMaintenance) {
				t.Fatal("new notification work must reject Closing", e)
			}
			if kind == "receipt" {
				if _, e = f.r.Delivery.RecordReceipt(f.service, notices[1], "close-new-ack", request); !errors.Is(e, maintenance.ErrMaintenance) {
					t.Fatal("new ACK must reject Closing", e)
				}
			}
			closeMustWait(t, f, plan)
			resume()
			e = closeResult(t, finished)
			f.r.Store.BeforeCommit = nil
			after := closeNotificationCounts(t, f)
			want := before
			if e == nil {
				if kind == "challenge" {
					want[0]++
				} else {
					want[1]++
					want[2]++
				}
			}
			if after != want {
				t.Fatal("owning final decision disagrees with durable facts", before, after, want, e)
			}
			closeDrained(t, f, plan)
			t.Log("actual owning SQL held, new entry denied, original final decision matched exact persisted counts; no new H")
		})
	}
	t.Run("committed-ACK-lost-transport-drain", func(t *testing.T) {
		f, _ := deliveryFragmentFixture(t, false)
		page, e := f.r.Delivery.GetNotifications(f.service, f.world.WorldInstanceID, "", "", 20)
		if e != nil || len(page.Notifications) != 2 {
			t.Fatal(e)
		}
		closeRetainPage(t, page)
		n := page.Notifications[0]
		input := closeReceiptInput(t, f, n)
		before := closeNotificationCounts(t, f)
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		resume := func() { once.Do(func() { close(release) }) }
		defer resume()
		lost := errors.New("synthetic transport lost after real receipt COMMIT")
		finished := make(chan error, 1)
		go func() {
			finished <- f.r.Delivery.WriteReceipt(f.service, string(n.NotificationRef), "close-lost-ack", input, func(raw []byte) error {
				if e := generated.Validate("IntegrationReceipt", raw); e != nil {
					return e
				}
				close(entered)
				<-release
				return lost
			})
		}()
		closeAwait(t, entered, "COMMIT then bounded transport")
		want := before
		want[1]++
		want[2]++
		if got := closeNotificationCounts(t, f); got != want || f.r.Store.DB.Stats().InUse != 0 {
			t.Fatal("actual committed receipt/ACKED and SQL returned prerequisite", got, want)
		}
		if e = f.r.Local.Coordinator.BeginClose(context.Background(), "close-ack-io"); e != nil {
			t.Fatal(e)
		}
		closeMustWait(t, f, "close-ack-io")
		resume()
		if e = closeResult(t, finished); !errors.Is(e, lost) {
			t.Fatal("lost response must remain a transport error", e)
		}
		if _, e = f.r.Delivery.RecordReceipt(f.service, string(n.NotificationRef), "close-lost-ack", input); !errors.Is(e, maintenance.ErrMaintenance) {
			t.Fatal("closed original replay must reject Closing", e)
		}
		if _, e = f.r.Delivery.GetNotifications(f.service, f.world.WorldInstanceID, string(n.NotificationRef), "", 1); !errors.Is(e, maintenance.ErrMaintenance) {
			t.Fatal("closed original lookup must reject Closing", e)
		}
		if got := closeNotificationCounts(t, f); got != want {
			t.Fatal("UNKNOWN lookup denial changed original COMMIT", got, want)
		}
		closeDrained(t, f, "close-ack-io")
		t.Log("caller UNKNOWN after real ACK COMMIT and lost response; Close denial is not absence; no resend")
	})
	t.Run("media-owning-positive", func(t *testing.T) {
		f, _ := deliveryFragmentFixture(t, false)
		if worked, e := f.r.Delivery.ProduceOneSyntheticMedia(context.Background()); e != nil || !worked {
			t.Fatal("actual native producer positive", worked, e)
		}
		var ready int
		if e := f.r.Store.DB.QueryRow("SELECT COUNT(*) FROM delivery_media WHERE state='READY'").Scan(&ready); e != nil || ready != 1 {
			t.Fatal("real READY", ready, e)
		}
	})
	for _, cancelIO := range []bool{false, true} {
		name := "media-reservation-release"
		if cancelIO {
			name = "media-reservation-cancel"
		}
		t.Run(name, func(t *testing.T) {
			f, _ := deliveryFragmentFixture(t, false)
			parent, done, e := f.r.Local.Participant.EnterDelivery(context.Background(), f.r.Store)
			if e != nil {
				t.Fatal(e)
			}
			defer done()
			claim, e := f.r.Store.ClaimSyntheticMedia(parent, f.r.Delivery)
			if e != nil {
				t.Fatal(e)
			}
			defer claim.Close()
			plan, e := f.r.Store.RuntimeMediaStagePlan(parent, f.r.Files, claim)
			if e != nil {
				t.Fatal(e)
			}
			reservation, e := localstate.Open(filepath.Join(f.r.Files.RootPath, "demo-local", "coffee", "runtime-media-locks", plan.StageStorage), true)
			if e != nil {
				t.Fatal(e)
			}
			ctx, cancel := context.WithTimeout(parent, 10*time.Second)
			defer cancel()
			unlock, e := reservation.Lock(ctx, "operations.lock")
			if e != nil {
				t.Fatal(e)
			}
			var once sync.Once
			release := func() { once.Do(unlock) }
			defer release()
			entered := make(chan struct{})
			var traced atomic.Bool
			f.r.Store.Trace = func(k store.LockKey) {
				if k.Rank == 110 && traced.CompareAndSwap(false, true) {
					close(entered)
				}
			}
			type outcome struct {
				stage *files.RuntimeMediaStage
				err   error
			}
			finished := make(chan outcome, 1)
			go func() { stage, e := f.r.Files.StageRuntimeTone(ctx, claim); finished <- outcome{stage, e} }()
			closeAwait(t, entered, "actual Files StagePlan")
			for f.r.Store.DB.Stats().InUse != 0 {
				select {
				case <-ctx.Done():
					t.Fatal("SQL not returned before native wait")
				case <-time.After(time.Millisecond):
				}
			}
			select {
			case o := <-finished:
				if o.stage != nil {
					o.stage.Close()
				}
				t.Fatal("native reservation was not held", o.err)
			default:
			}
			if e = f.r.Local.Coordinator.BeginClose(context.Background(), name); e != nil {
				t.Fatal(e)
			}
			if _, e = f.r.Delivery.ProduceOneSyntheticMedia(context.Background()); !errors.Is(e, maintenance.ErrMaintenance) {
				t.Fatal("new media work must reject Closing", e)
			}
			closeMustWait(t, f, name)
			if cancelIO {
				cancel()
			} else {
				release()
			}
			var got outcome
			select {
			case got = <-finished:
			case <-time.After(5 * time.Second):
				t.Fatal("actual Files call did not return")
			}
			f.r.Store.Trace = nil
			if got.stage != nil {
				got.stage.Close()
			}
			wantErr := maintenance.ErrMaintenance
			if cancelIO {
				wantErr = context.Canceled
			}
			if !errors.Is(got.err, wantErr) {
				t.Fatal("actual media reservation exit cause", got.err, wantErr)
			}
			release()
			if e = claim.Close(); e != nil {
				t.Fatal(e)
			}
			done()
			file := filepath.Join(f.r.Files.RootPath, "demo-local", "coffee", "runtime-media", plan.StageStorage+".wav")
			if _, e = os.Lstat(file); !os.IsNotExist(e) {
				t.Fatal("late writer produced a WAV", e)
			}
			closeDrained(t, f, name)
			t.Log("real Files reservation wait with SQL returned; Close/cancel returned actual claim and parent; no late WAV")
		})
	}
}

func closeRetainPage(t *testing.T, p generated.IntegrationNotificationPage) {
	t.Helper()
	retainSessionScanMaterial(t, p.NextCursor)
	for _, n := range p.Notifications {
		retainSessionScanMaterial(t, n.Challenge.Nonce)
	}
}

func closeReceiptInput(t *testing.T, f integrationFixFixture, n generated.IntegrationNotification) generated.IntegrationReceiptRequest {
	t.Helper()
	var installation, customer string
	if e := f.r.Store.DB.QueryRow("SELECT installation_id FROM memory_installation WHERE singleton=1").Scan(&installation); e != nil {
		t.Fatal(e)
	}
	if e := f.r.Store.DB.QueryRow("SELECT customer_id FROM integration_notifications WHERE notice_ref=?", n.NotificationRef).Scan(&customer); e != nil {
		t.Fatal(e)
	}
	raw, e := json.Marshal([7]string{installation, customer, string(n.ConsumerRef), string(n.NotificationRef), string(n.NotificationRevision), string(n.MessageRef), n.SafeSummary})
	if e != nil {
		t.Fatal(e)
	}
	hash := sha256.Sum256(raw)
	return generated.IntegrationReceiptRequest{ChallengeNonce: n.Challenge.Nonce, ChallengeRef: n.Challenge.ChallengeRef, CustomerMessageHash: hex.EncodeToString(hash[:]), CustomerMessageRef: n.MessageRef, CustomerStatus: "PERSISTED", NotificationRevision: n.NotificationRevision, PayloadHash: n.PayloadHash}
}

func closeNotificationCounts(t *testing.T, f integrationFixFixture) [5]uint64 {
	t.Helper()
	var v [5]uint64
	if e := f.r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM integration_challenges),(SELECT COUNT(*) FROM integration_receipts),(SELECT COUNT(*) FROM outbox WHERE status='ACKED'),(SELECT COUNT(*) FROM integration_notifications),(SELECT update_seq FROM delivery_streams WHERE world_instance_id=?)", f.world.WorldInstanceID).Scan(&v[0], &v[1], &v[2], &v[3], &v[4]); e != nil {
		t.Fatal(e)
	}
	return v
}

func closeAwait(t *testing.T, ch <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(4 * time.Second):
		t.Fatal("not reached", label)
	}
}
func closeResult(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case e := <-ch:
		return e
	case <-time.After(5 * time.Second):
		t.Fatal("owning operation not returned")
		return nil
	}
}
func closeMustWait(t *testing.T, f integrationFixFixture, plan string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := f.r.Local.Coordinator.Drain(ctx, plan)
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatal("Drain must exhaust this deadline while real owning work is held", err, ctx.Err())
	}
	t.Log("Drain held until its own deadline", time.Since(started))
}
func closeDrained(t *testing.T, f integrationFixFixture, plan string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if e := f.r.Local.Coordinator.Drain(ctx, plan); e != nil {
		t.Fatal("Drain after actual owned return", e)
	}
}
