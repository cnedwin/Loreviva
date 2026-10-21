package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"loreviva.local/loreviva/internal/delivery"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/files"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/localstate"
	store "loreviva.local/loreviva/internal/store/mysql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

// These controls reach the original owning callers before mutating only the
// persisted shape. Natural lease expiry is not itself a playback revocation.
func TestIntegrationMediaShapeOwningCallers(t *testing.T) {
	t.Run("READY-current-and-final", func(t *testing.T) {
		f, _ := deliveryFragmentFixture(t, false)
		if worked, e := f.r.Delivery.ProduceOneSyntheticMedia(context.Background()); e != nil || !worked {
			t.Fatal("real READY prerequisite", worked, e)
		}
		var asset string
		var accepted, lease time.Time
		if e := f.r.Store.DB.QueryRow("SELECT asset_id,accepted_at,lease_expires_at FROM delivery_media WHERE world_instance_id=? AND state='READY'", f.world.WorldInstanceID).Scan(&asset, &accepted, &lease); e != nil {
			t.Fatal(e)
		}
		read := func(t *testing.T, want bool) {
			t.Helper()
			v, e := f.r.Delivery.ReadRuntimeMedia(f.client, asset, delivery.MediaRange{})
			if v != nil {
				defer v.Close()
			}
			if want {
				if e != nil || v == nil {
					t.Fatal("lawful READY read", e)
				}
			} else if e == nil || v != nil {
				t.Error("CAUSAL_RED: malformed READY returned authorized media")
			}
		}
		read(t, true)
		for _, tc := range []struct {
			name  string
			value any
		}{{"NULL-lease", nil}, {"lease-at-acceptance", accepted}} {
			t.Run(tc.name, func(t *testing.T) {
				r, e := f.r.Store.DB.Exec("UPDATE delivery_media SET lease_expires_at=? WHERE asset_id=?", tc.value, asset)
				if e != nil {
					t.Fatal("fault prerequisite", e)
				}
				n, e := r.RowsAffected()
				if e != nil || n != 1 {
					t.Fatal("fault row", n, e)
				}
				defer func() {
					if _, e := f.r.Store.DB.Exec("UPDATE delivery_media SET lease_expires_at=? WHERE asset_id=?", lease, asset); e != nil {
						t.Error("restore", e)
					}
				}()
				read(t, false)
			})
		}
		t.Run("final-drift-rolls-back", func(t *testing.T) {
			reached := false
			f.r.Store.BeforeReadFinish = func(ctx context.Context, tx *sql.Tx) error {
				reached = true
				r, e := tx.ExecContext(ctx, "UPDATE delivery_media SET lease_expires_at=NULL WHERE asset_id=?", asset)
				if e != nil {
					return e
				}
				n, e := r.RowsAffected()
				if e != nil || n != 1 {
					return fmt.Errorf("fault affected %d: %v", n, e)
				}
				return nil
			}
			read(t, false)
			f.r.Store.BeforeReadFinish = nil
			if !reached {
				t.Fatal("final hook not reached")
			}
			var got time.Time
			if e := f.r.Store.DB.QueryRow("SELECT lease_expires_at FROM delivery_media WHERE asset_id=?", asset).Scan(&got); e != nil || !got.Equal(lease) {
				t.Fatal("final rollback", e)
			}
		})
		mediaWaitDBUTC(t, f.r.Store.DB, lease)
		read(t, true)
	})
	t.Run("expired-PENDING-before-reclaim", func(t *testing.T) {
		f, _ := deliveryFragmentFixture(t, false)
		claim := func() (*store.RuntimeMediaClaim, error) {
			ctx, done, e := f.r.Local.Participant.EnterDelivery(context.Background(), f.r.Store)
			if e != nil {
				return nil, e
			}
			defer done()
			return f.r.Store.ClaimSyntheticMedia(ctx, f.r.Delivery)
		}
		first, e := claim()
		if e != nil {
			t.Fatal("real first PENDING", e)
		}
		first.Close()
		var asset string
		var lease, deadline time.Time
		var fence, attempts uint64
		if e := f.r.Store.DB.QueryRow("SELECT asset_id,lease_expires_at,execute_by,fence,attempt_count FROM delivery_media WHERE world_instance_id=?", f.world.WorldInstanceID).Scan(&asset, &lease, &deadline, &fence, &attempts); e != nil {
			t.Fatal(e)
		}
		if fence != 1 || attempts != 1 {
			t.Fatal("first owning tuple")
		}
		mediaWaitDBUTC(t, f.r.Store.DB, lease)
		r, e := f.r.Store.DB.Exec("UPDATE delivery_media SET fence=2 WHERE asset_id=?", asset)
		if e != nil {
			t.Fatal("fault prerequisite", e)
		}
		n, e := r.RowsAffected()
		if e != nil || n != 1 {
			t.Fatal("fault row", n, e)
		}
		var high uint64
		if e = f.r.Store.DB.QueryRow("SELECT MAX(update_seq) FROM delivery_updates WHERE world_instance_id=?", f.world.WorldInstanceID).Scan(&high); e != nil {
			t.Fatal(e)
		}
		second, e := claim()
		if second != nil {
			second.Close()
		}
		if e == nil || second != nil {
			t.Fatal("CAUSAL_RED: malformed expired PENDING was reclaimed")
		}
		var gotFence, gotAttempts, gotHigh uint64
		var gotDeadline time.Time
		if e = f.r.Store.DB.QueryRow("SELECT fence,attempt_count,execute_by,(SELECT MAX(update_seq) FROM delivery_updates WHERE world_instance_id=?) FROM delivery_media WHERE asset_id=?", f.world.WorldInstanceID, asset).Scan(&gotFence, &gotAttempts, &gotDeadline, &gotHigh); e != nil {
			t.Fatal(e)
		}
		if gotFence != 2 || gotAttempts != 1 || !gotDeadline.Equal(deadline) || gotHigh != high {
			t.Fatal("rejected reclaim wrote row or H")
		}
		if _, e = f.r.Store.DB.Exec("UPDATE delivery_media SET fence=1 WHERE asset_id=?", asset); e != nil {
			t.Fatal(e)
		}
		second, e = claim()
		if e != nil || second == nil {
			t.Fatal("lawful naturally expired PENDING", e)
		}
		second.Close()
	})
}

func TestIntegrationRuntimeMediaAuthorizationAndRange(t *testing.T) {
	f, task := deliveryFragmentFixture(t, false)
	parent, release, e := f.r.Local.Participant.EnterDelivery(context.Background(), f.r.Store)
	if e != nil {
		t.Fatal(e)
	}
	claim, e := f.r.Store.ClaimSyntheticMedia(parent, f.r.Delivery)
	release()
	if e != nil {
		t.Fatal("real PENDING admission", e)
	}
	defer claim.Close()
	var asset, storage string
	if e := f.r.Store.DB.QueryRow("SELECT asset_id,COALESCE(runtime_file_ref,'') FROM delivery_media WHERE world_instance_id=? AND state='PENDING'", f.world.WorldInstanceID).Scan(&asset, &storage); e != nil {
		t.Fatal(e)
	}
	type reader struct {
		ctx            context.Context
		token, profile string
	}
	readers := []reader{}
	for i, profile := range []string{"integration_r0_v1", "public_v1"} {
		session, e := f.r.Store.ExchangeIntegrationSession(f.service, fmt.Sprintf("media-read-%d", i), profile, generated.ClientSessionRequest{ProductID: "coffee", WorldInstanceID: f.world.WorldInstanceID, ExternalSubjectID: "S66-player-external", EntitlementReference: "s66-entitlement-one", RequestedActions: []string{"READ"}})
		if e != nil {
			t.Fatal("real CLIENT", e)
		}
		retainSessionScanMaterial(t, session.AccessToken)
		c, _, e := f.r.Store.AuthenticateIntegration(context.Background(), "Bearer "+session.AccessToken, domain.IntegrationClient)
		if e != nil {
			t.Fatal(e)
		}
		readers = append(readers, reader{c, session.AccessToken, profile})
	}
	t.Run("profiles_real_nonready", func(t *testing.T) {
		for _, r := range readers {
			var body []byte
			if e := f.r.Delivery.WriteTurn(r.ctx, task, r.profile, func(b []byte) error { body = append([]byte(nil), b...); return nil }); e != nil {
				t.Fatal(e)
			}
			var v struct {
				ApprovedPackets []struct {
					Media []map[string]any `json:"media"`
				} `json:"approved_packets"`
			}
			if json.Unmarshal(body, &v) != nil || len(v.ApprovedPackets) != 1 || len(v.ApprovedPackets[0].Media) != 1 {
				t.Fatal("real nonREADY")
			}
			m := v.ApprovedPackets[0].Media[0]
			if len(m) != 4 || m["media_state"] != "PENDING" || m["resource_revision"] != "2" {
				t.Fatal("nonREADY no asset or extras", r.profile)
			}
			if _, e := f.r.Delivery.ReadRuntimeMedia(r.ctx, asset, delivery.MediaRange{}); e == nil {
				t.Fatal("PENDING playback")
			}
		}
	})
	parent, release, e = f.r.Local.Participant.EnterDelivery(context.Background(), f.r.Store)
	if e != nil {
		t.Fatal(e)
	}
	stage, e := f.r.Files.StageRuntimeTone(parent, claim)
	if e != nil {
		release()
		t.Fatal(e)
	}
	defer stage.Close()
	ev, e := f.r.Files.SealRuntimeMediaStage(parent, claim, stage)
	if e != nil {
		release()
		t.Fatal(e)
	}
	pin, e := f.r.Files.AcquireMediaFinalEvidence(claim, ev)
	if e != nil {
		release()
		t.Fatal(e)
	}
	finish := f.r.Store.FinishSyntheticMedia(parent, f.r.Delivery, claim, pin)
	complete := f.r.Files.CompleteMediaFinalEvidence(parent, claim, pin)
	release()
	if finish != nil || complete != nil {
		t.Fatal("actual READY", finish, complete)
	}
	if e = f.r.Store.DB.QueryRow("SELECT runtime_file_ref FROM delivery_media WHERE asset_id=?", asset).Scan(&storage); e != nil {
		t.Fatal(e)
	}
	srv := httptest.NewServer(f.r.Handler())
	defer srv.Close()
	client := *srv.Client()
	client.Timeout = 30 * time.Second
	request := func(t *testing.T, path, rng, token, profile string) (int, http.Header, []byte) {
		t.Helper()
		req, e := http.NewRequest("GET", srv.URL+path, nil)
		if e != nil {
			t.Fatal(e)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if profile != "" {
			req.Header.Set("Loreviva-Client-Contract", profile)
		}
		if rng != "" {
			req.Header.Set("Range", rng)
		}
		resp, e := client.Do(req)
		if e != nil {
			t.Fatal("actual loopback", e)
		}
		body, e := io.ReadAll(resp.Body)
		ce := resp.Body.Close()
		if e != nil || ce != nil {
			t.Fatal(e, ce)
		}
		return resp.StatusCode, resp.Header, body
	}
	path := "/world/v1/runtime-media/" + asset + "/content"
	t.Run("real_200_206_416", func(t *testing.T) {
		for _, c := range []struct {
			r         string
			status, n int
		}{{"", 200, 160044}, {"bytes=0-43", 206, 44}, {"bytes=-8", 206, 8}, {"bytes=160040-", 206, 4}, {"bytes=160044-", 416, 0}, {"bytes=0-1,4-5", 416, 0}, {"bytes=-0", 416, 0}} {
			code, h, body := request(t, path, c.r, readers[0].token, readers[0].profile)
			if code != c.status {
				t.Fatalf("range %q status %d", c.r, code)
			}
			if c.n > 0 && (len(body) != c.n || h.Get("Content-Type") != "audio/wav" || h.Get("Cache-Control") != "no-store") {
				t.Fatalf("range body/header %q n%d", c.r, len(body))
			}
			if c.r == "" && (string(body[:4]) != "RIFF" || string(body[8:12]) != "WAVE") {
				t.Fatal("actual WAV")
			}
			if code == 416 && h.Get("Content-Range") != "bytes */160044" {
				t.Fatal("unsatisfied content range")
			}
		}
	})
	t.Run("profiles_real_ready", func(t *testing.T) {
		for _, r := range readers {
			var body []byte
			if e := f.r.Delivery.WriteTurn(r.ctx, task, r.profile, func(b []byte) error { body = append([]byte(nil), b...); return nil }); e != nil {
				t.Fatal(r.profile, e)
			}
			var v struct {
				ApprovedPackets []struct {
					Media []map[string]any `json:"media"`
				} `json:"approved_packets"`
			}
			if json.Unmarshal(body, &v) != nil || len(v.ApprovedPackets) != 1 || len(v.ApprovedPackets[0].Media) != 1 {
				t.Fatal("actual media projection")
			}
			m := v.ApprovedPackets[0].Media[0]
			if m["media_state"] != "READY" || m["asset_id"] != asset || m["resource_revision"] != "3" {
				t.Fatal("media identity")
			}
			expected := 5
			if r.profile == "integration_r0_v1" {
				expected = 9
				if m["byte_length"] != "160044" || m["content_path"] != path || m["mime_type"] != "audio/wav" {
					t.Fatal("integration extras")
				}
			}
			if len(m) != expected {
				t.Fatal("closed profile field count", r.profile, len(m))
			}
			code, _, b := request(t, path, "bytes=0-43", r.token, r.profile)
			if code != 206 || len(b) != 44 {
				t.Fatal("profile actual asset", code)
			}
		}
	})
	t.Run("not_URL_or_foreign_authority", func(t *testing.T) {
		code, _, _ := request(t, path+"?access_token="+readers[0].token, "bytes=0-43", "", readers[0].profile)
		if code == 200 || code == 206 {
			t.Fatal("URL bearer")
		}
		code, _, _ = request(t, path, "bytes=0-43", readers[0].token, "public_v1")
		if code != 409 {
			t.Fatal("profile mismatch", code)
		}
		if _, e := f.r.Delivery.ReadRuntimeMedia(f.service, asset, delivery.MediaRange{}); e == nil {
			t.Fatal("SERVICE body")
		}
		if _, e := f.r.Delivery.ReadRuntimeMedia(f.client, "foreign-asset", delivery.MediaRange{}); e == nil {
			t.Fatal("foreign asset")
		}
		if _, e := (&delivery.AuthorizedMedia{}).ReadChunk(readers[0].ctx, 44); e == nil {
			t.Fatal("zero seal")
		}
	})
	t.Run("physical_loss_and_hash", func(t *testing.T) {
		file := filepath.Join(f.r.Files.RootPath, "demo-local", "coffee", "runtime-media", storage+".wav")
		raw, e := os.ReadFile(file)
		if e != nil {
			t.Fatal(e)
		}
		for _, fault := range []string{"missing", "corrupt"} {
			saved := file + ".owned-saved"
			if fault == "missing" {
				if e = os.Rename(file, saved); e != nil {
					t.Fatal(e)
				}
			} else {
				changed := append([]byte(nil), raw...)
				changed[100] ^= 1
				if e = os.WriteFile(file, changed, 0600); e != nil {
					t.Fatal(e)
				}
			}
			m, e := f.r.Delivery.ReadRuntimeMedia(readers[0].ctx, asset, delivery.MediaRange{})
			if e != nil {
				t.Fatal("SQL eligibility remains legitimate", e)
			}
			_, e = m.ReadChunk(readers[0].ctx, 44)
			ce := m.Close()
			if fault == "missing" {
				if er := os.Rename(saved, file); er != nil {
					t.Fatal(er)
				}
			} else if er := os.WriteFile(file, raw, 0600); er != nil {
				t.Fatal(er)
			}
			if e == nil || ce != nil {
				t.Fatal("actual file denial", fault, e, ce)
			}
		}
	})
	t.Run("final_row_drift_zero_output", func(t *testing.T) {
		reached := false
		f.r.Store.BeforeReadFinish = func(c context.Context, tx *sql.Tx) error {
			reached = true
			r, e := tx.ExecContext(c, "UPDATE delivery_media SET mime_type='invalid' WHERE asset_id=?", asset)
			if e != nil {
				return e
			}
			n, e := r.RowsAffected()
			if e != nil {
				return e
			}
			if n != 1 {
				return errors.New("fault row missing")
			}
			return nil
		}
		m, e := f.r.Delivery.ReadRuntimeMedia(readers[0].ctx, asset, delivery.MediaRange{})
		f.r.Store.BeforeReadFinish = nil
		if e == nil || m != nil || !reached {
			t.Fatal("exact final refusal", e, reached)
		}
		var mime string
		if e = f.r.Store.DB.QueryRow("SELECT mime_type FROM delivery_media WHERE asset_id=?", asset).Scan(&mime); e != nil || mime != "audio/wav" {
			t.Fatal("read rollback", mime, e)
		}
	})
	t.Run("close_inside_bounded_chunk", func(t *testing.T) {
		m, e := f.r.Delivery.ReadRuntimeMedia(readers[0].ctx, asset, delivery.MediaRange{})
		if e != nil {
			t.Fatal(e)
		}
		closeDone := make(chan error, 1)
		calls := 0
		var closeReturned bool
		e = m.WriteChunks(readers[0].ctx, func(b []byte) error {
			calls++
			if len(b) > 16384 || f.r.Store.DB.Stats().InUse != 0 {
				t.Error("chunk bound or SQL still live")
			}
			go func() { closeDone <- m.Close() }()
			select {
			case ce := <-closeDone:
				closeReturned = ce == nil
			case <-time.After(500 * time.Millisecond):
			}
			return io.ErrClosedPipe
		})
		if !closeReturned {
			if ce := <-closeDone; ce != nil {
				t.Fatal(ce)
			}
		}
		if e == nil || calls != 1 || !closeReturned {
			t.Fatal("CAUSAL_RED: reentrant Close must return without waiting on callback", calls, closeReturned, e)
		}
	})
	t.Run("actual_revoke_between_chunks", func(t *testing.T) {
		m, e := f.r.Delivery.ReadRuntimeMedia(readers[0].ctx, asset, delivery.MediaRange{})
		if e != nil {
			t.Fatal(e)
		}
		defer m.Close()
		calls := 0
		e = m.WriteChunks(readers[0].ctx, func(b []byte) error {
			calls++
			if len(b) > 16384 || f.r.Store.DB.Stats().InUse != 0 {
				return errors.New("bounded chunk SQL overlap")
			}
			_, er := f.r.Store.ApplyPlayerSelfConsent(f.client, "media-revoke", json.RawMessage(`{"command":"REVOKE","expected_revision":"1","policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`))
			return er
		})
		if e == nil || calls != 1 {
			t.Fatal("next chunk after real revoke", calls, e)
		}
	})
}

func TestIntegrationMediaLatePacketRevision(t *testing.T) {
	f, task := deliveryFragmentFixture(t, true)
	before, e := f.r.Delivery.GetTurn(f.client, task)
	if e != nil || len(before.ApprovedPackets) != 1 || len(before.ApprovedPackets[0].Media) != 0 {
		t.Fatal("original empty-media packet", e)
	}
	first, e := f.r.Delivery.GetEventsPage(f.client, f.world.WorldInstanceID, "", 1)
	if e != nil {
		t.Fatal(e)
	}
	retainSessionScanMaterial(t, first.NextCursor)
	partial, e := f.r.Delivery.GetEventsPage(f.client, f.world.WorldInstanceID, first.NextCursor, 1)
	if e != nil || len(partial.Events) != 1 || len(partial.Deliveries) != 0 {
		t.Fatal("real ACTION_BUNDLE fragment", e)
	}
	retainSessionScanMaterial(t, partial.NextCursor)
	decode, _ := deliveryCursorCodec(t, f)
	originalCursor := decode(partial.NextCursor)
	parent, done, e := f.r.Local.Participant.EnterDelivery(context.Background(), f.r.Store)
	if e != nil {
		t.Fatal(e)
	}
	claim, e := f.r.Store.ClaimSyntheticMedia(parent, f.r.Delivery)
	done()
	if e != nil {
		t.Fatal("real initial pending", e)
	}
	defer claim.Close()
	var pendingRaw string
	if e = f.r.Store.DB.QueryRow("SELECT CAST(JSON_ARRAY(update_seq,business_update_id,delivery_state,resource_revision,event_refs,packet_ref,audience_refs,lineage_refs) AS CHAR) FROM delivery_updates WHERE task_ref=? AND kind='MEDIA' AND delivery_state='PENDING'", task).Scan(&pendingRaw); e != nil {
		t.Fatal(e)
	}
	pending, e := f.r.Delivery.GetTurn(f.client, task)
	if e != nil || len(pending.ApprovedPackets) != 1 || len(pending.ApprovedPackets[0].Media) != 1 {
		t.Fatal("real PENDING projection", e)
	}
	var pm map[string]any
	if json.Unmarshal(pending.ApprovedPackets[0].Media[0], &pm) != nil || len(pm) != 4 || pm["media_state"] != "PENDING" || pm["resource_revision"] != "2" {
		t.Fatal("nonREADY closed fields")
	}
	parent, done, e = f.r.Local.Participant.EnterDelivery(context.Background(), f.r.Store)
	if e != nil {
		t.Fatal(e)
	}
	stage, e := f.r.Files.StageRuntimeTone(parent, claim)
	if e != nil {
		done()
		t.Fatal(e)
	}
	defer stage.Close()
	ev, e := f.r.Files.SealRuntimeMediaStage(parent, claim, stage)
	if e != nil {
		done()
		t.Fatal(e)
	}
	pin, e := f.r.Files.AcquireMediaFinalEvidence(claim, ev)
	if e != nil {
		done()
		t.Fatal(e)
	}
	finish := f.r.Store.FinishSyntheticMedia(parent, f.r.Delivery, claim, pin)
	complete := f.r.Files.CompleteMediaFinalEvidence(parent, claim, pin)
	done()
	if finish != nil || complete != nil {
		t.Fatal("actual READY", finish, complete)
	}
	after, e := f.r.Delivery.GetTurn(f.client, task)
	if e != nil || len(after.ApprovedPackets) != 1 || len(after.ApprovedPackets[0].Media) != 1 {
		t.Fatal("current latest packet", e)
	}
	var media map[string]any
	if json.Unmarshal(after.ApprovedPackets[0].Media[0], &media) != nil || media["media_state"] != "READY" || media["resource_revision"] != "3" {
		t.Fatal("latest revision projection")
	}
	third, e := f.r.Delivery.GetEventsPage(f.client, f.world.WorldInstanceID, partial.NextCursor, 1)
	if e != nil || len(third.Events) != 0 || len(third.Deliveries) != 1 {
		t.Fatal("original partial survives actual MEDIA", e)
	}
	retainSessionScanMaterial(t, third.NextCursor)
	cursor := decode(third.NextCursor)
	if cursor["Issued"] != originalCursor["Issued"] || cursor["Expires"] != originalCursor["Expires"] {
		t.Fatal("expiry renewed")
	}
	history, e := f.r.Delivery.GetEventsPage(f.client, f.world.WorldInstanceID, third.NextCursor, 50)
	if e != nil || len(history.Deliveries) != 2 {
		t.Fatal("both historical MEDIA states", len(history.Deliveries), e)
	}
	retainSessionScanMaterial(t, history.NextCursor)
	for i, state := range []string{"PENDING", "READY"} {
		var d map[string]any
		if json.Unmarshal(history.Deliveries[i], &d) != nil || d["media_state"] != state || d["packet_revision"] != fmt.Sprint(i+2) {
			t.Fatal("historical revision", i)
		}
	}
	var retainedRaw string
	if e = f.r.Store.DB.QueryRow("SELECT CAST(JSON_ARRAY(update_seq,business_update_id,delivery_state,resource_revision,event_refs,packet_ref,audience_refs,lineage_refs) AS CHAR) FROM delivery_updates WHERE task_ref=? AND kind='MEDIA' AND delivery_state='PENDING'", task).Scan(&retainedRaw); e != nil || retainedRaw != pendingRaw {
		t.Fatal("original PENDING rewritten", e)
	}
	var calls, commits, notices int
	if e = f.r.Store.DB.QueryRow("SELECT calls_used,(SELECT COUNT(*) FROM harness_turn_commits WHERE task_id=?),(SELECT COUNT(*) FROM integration_notifications WHERE result_ref=?) FROM tasks WHERE task_id=?", task, after.ApprovedPackets[0].PacketID, task).Scan(&calls, &commits, &notices); e != nil || calls != 1 || commits != 1 || notices != 1 {
		t.Fatal("business replay", e, calls, commits, notices)
	}
	t.Run("historical_tuple_fault", func(t *testing.T) {
		res, e := f.r.Store.DB.Exec("UPDATE delivery_updates SET resource_revision=9 WHERE task_ref=? AND kind='MEDIA' AND delivery_state='PENDING'", task)
		if e != nil {
			t.Fatal(e)
		}
		n, e := res.RowsAffected()
		if e != nil || n != 1 {
			t.Fatal("fault reach", n, e)
		}
		_, e = f.r.Delivery.GetEventsPage(f.client, f.world.WorldInstanceID, third.NextCursor, 50)
		restore, er := f.r.Store.DB.Exec("UPDATE delivery_updates SET resource_revision=2 WHERE task_ref=? AND kind='MEDIA' AND delivery_state='PENDING'", task)
		if er != nil {
			t.Fatal(er)
		}
		n, er = restore.RowsAffected()
		if er != nil || n != 1 {
			t.Fatal("restore", n, er)
		}
		if e == nil {
			t.Fatal("historical wrong revision accepted")
		}
	})
	t.Run("missing_current_file_preserves_history", func(t *testing.T) {
		var storage string
		if e := f.r.Store.DB.QueryRow("SELECT runtime_file_ref FROM delivery_media WHERE world_instance_id=?", f.world.WorldInstanceID).Scan(&storage); e != nil {
			t.Fatal(e)
		}
		file := filepath.Join(f.r.Files.RootPath, "demo-local", "coffee", "runtime-media", storage+".wav")
		saved := file + ".history-test"
		if e := os.Rename(file, saved); e != nil {
			t.Fatal(e)
		}
		defer func() {
			if e := os.Rename(saved, file); e != nil {
				t.Error(e)
			}
		}()
		page, e := f.r.Delivery.GetEventsPage(f.client, f.world.WorldInstanceID, third.NextCursor, 50)
		if e != nil || len(page.Deliveries) != 2 {
			t.Fatal("CAUSAL_RED: file absence is current unavailable, not erased historical rows", e)
		}
		var d map[string]any
		if json.Unmarshal(page.Deliveries[1], &d) != nil || d["media_state"] != "READY" || d["delivery_state"] != "WITHDRAWN" || d["packet_revision"] != "3" || d["available_packet"] != nil {
			t.Fatal("historical READY no current playback")
		}
	})
	t.Log("MEDIA single-ordinal interior partial: UNREACHABLE_WITH_SINGLE_ORDINAL; no cursor fabricated")
}

// The prerequisite is the original owning TURN COMMIT, not an injected sidecar.
// Keep this dynamic boundary so absence is a reached capability failure rather
// than a missing-symbol compiler error.
func TestIntegrationSyntheticMediaOwningLifecycle(t *testing.T) {
	t.Run("actual_owning_commit_and_wav", mediaOwningHappy)
	t.Run("hash_read_outside_SQL_and_pure_final", mediaPhaseIsolation)
	for _, kind := range []string{"zero_copy_foreign", "missing_original", "seal_loss", "final_close", "final_row_drift", "final_H_drift"} {
		t.Run(kind, func(t *testing.T) { mediaOwningFailure(t, kind) })
	}
	for _, kind := range []string{"empty", "oversize", "nonregular", "alias", "replacement", "size_between_observations"} {
		t.Run("original_metadata_"+kind, func(t *testing.T) { mediaOriginalMetadata(t, kind) })
	}
}

// This ordinary context blocks at the real contextReader's first Read, after
// Seal's initial cancellation check. It neither mints permission nor replaces IO.
type mediaReadPause struct {
	context.Context
	calls           atomic.Uint32
	reached, resume chan struct{}
}

func (p *mediaReadPause) Err() error {
	if p.calls.Add(1) == 2 {
		close(p.reached)
		select {
		case <-p.resume:
		case <-p.Context.Done():
		}
	}
	return p.Context.Err()
}

func mediaPhaseIsolation(t *testing.T) {
	f, _ := deliveryFragmentFixture(t, false)
	ctx, done, err := f.r.Local.Participant.EnterDelivery(context.Background(), f.r.Store)
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	c, err := f.r.Store.ClaimSyntheticMedia(ctx, f.r.Delivery)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	stage, err := f.r.Files.StageRuntimeTone(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	defer stage.Close()
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	pause := &mediaReadPause{Context: bounded, reached: make(chan struct{}), resume: make(chan struct{})}
	type result struct {
		ev  *files.RuntimeMediaEvidence
		err error
	}
	finished := make(chan result, 1)
	go func() { ev, e := f.r.Files.SealRuntimeMediaStage(pause, c, stage); finished <- result{ev, e} }()
	select {
	case <-pause.reached:
	case <-bounded.Done():
		t.Fatal("actual hash reader not reached", bounded.Err())
	}
	before, err := stage.ObserveIO()
	if err != nil {
		close(pause.resume)
		<-finished
		t.Fatal(err)
	}
	inUse := f.r.Store.DB.Stats().InUse
	close(pause.resume)
	r := <-finished
	if inUse != 0 || before.BytesRead != 0 || r.err != nil {
		t.Fatal("P1 held SQL or did not pause before actual read", inUse, before, r.err)
	}
	sealed, err := stage.ObserveIO()
	if err != nil || sealed.BytesRead != 160044 || sealed.FileCalls <= before.FileCalls {
		t.Fatal("actual complete WAV read", sealed, err)
	}
	pin, err := f.r.Files.AcquireMediaFinalEvidence(c, r.ev)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if e := f.r.Files.CompleteMediaFinalEvidence(context.WithoutCancel(ctx), c, pin); e != nil {
			t.Error(e)
		}
	}()
	hook := false
	f.r.Store.BeforeCommit = func(_ context.Context, _ *sql.Tx) error {
		hook = true
		now, e := stage.ObserveIO()
		if e != nil || now != sealed || f.r.Store.DB.Stats().InUse != 1 {
			return errors.New("P2 executed file API or lacks owning SQL")
		}
		return nil
	}
	err = f.r.Store.FinishSyntheticMedia(ctx, f.r.Delivery, c, pin)
	f.r.Store.BeforeCommit = nil
	after, oe := stage.ObserveIO()
	if err != nil || oe != nil || !hook || after != sealed || f.r.Store.DB.Stats().InUse != 0 {
		t.Fatal("pure final or SQL return", hook, after, err, oe)
	}
	t.Logf("P1 real hash reader paused with SQL InUse=%d; actual bytes=%d; P2 explicit owned-file API delta=%d", inUse, sealed.BytesRead, after.FileCalls-sealed.FileCalls)
}

// A genuine DBUTC lease expires without rewriting dates or recreating the
// fixture. One acceptance survives the second fence and its absolute deadline.
func TestIntegrationMediaLeaseFenceAndDeadline(t *testing.T) {
	t.Run("natural_two_claims_absolute_deadline", func(t *testing.T) {
		f, _ := deliveryFragmentFixture(t, false)
		ctx, done, err := f.r.Local.Participant.EnterDelivery(context.Background(), f.r.Store)
		if err != nil {
			t.Fatal(err)
		}
		defer done()
		c1, err := f.r.Store.ClaimSyntheticMedia(ctx, f.r.Delivery)
		if err != nil {
			t.Fatal(err)
		}
		defer c1.Close()
		p1, err := f.r.Store.RuntimeMediaStagePlan(ctx, f.r.Files, c1)
		if err != nil {
			t.Fatal(err)
		}
		stage, err := f.r.Files.StageRuntimeTone(ctx, c1)
		if err != nil {
			t.Fatal(err)
		}
		defer stage.Close()
		ev, err := f.r.Files.SealRuntimeMediaStage(ctx, c1, stage)
		if err != nil {
			t.Fatal(err)
		}
		pin, err := f.r.Files.AcquireMediaFinalEvidence(c1, ev)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if e := f.r.Files.CompleteMediaFinalEvidence(context.WithoutCancel(ctx), c1, pin); e != nil {
				t.Error(e)
			}
		}()
		var accepted, deadline, lease time.Time
		var h uint64
		if err = f.r.Store.DB.QueryRow("SELECT m.accepted_at,m.execute_by,m.lease_expires_at,s.update_seq FROM delivery_media m JOIN delivery_streams s USING(tenant_id,product_id,world_instance_id) WHERE m.asset_id=?", p1.Asset).Scan(&accepted, &deadline, &lease, &h); err != nil {
			t.Fatal(err)
		}
		if deadline.Sub(accepted) != 60*time.Second || lease.Sub(accepted) != 30*time.Second {
			t.Fatal("actual initial temporal tuple")
		}
		if _, err = f.r.Store.ClaimSyntheticMedia(ctx, f.r.Delivery); !errors.Is(err, store.ErrNoTask) {
			t.Fatal("live lease repeated", err)
		}
		mediaWaitDBUTC(t, f.r.Store.DB, lease)
		c2, err := f.r.Store.ClaimSyntheticMedia(ctx, f.r.Delivery)
		if err != nil {
			t.Fatal("natural second claim", err)
		}
		defer c2.Close()
		p2, err := f.r.Store.RuntimeMediaStagePlan(ctx, f.r.Files, c2)
		if err != nil || p2.Fence != 2 || p2.Asset != p1.Asset || p2.StageStorage == p1.StageStorage {
			t.Fatal("second sealed fence", err)
		}
		var a2, d2, l2 time.Time
		var h2, rev, attempt uint64
		if err = f.r.Store.DB.QueryRow("SELECT m.accepted_at,m.execute_by,m.lease_expires_at,s.update_seq,m.revision,m.attempt_count FROM delivery_media m JOIN delivery_streams s USING(tenant_id,product_id,world_instance_id) WHERE m.asset_id=?", p1.Asset).Scan(&a2, &d2, &l2, &h2, &rev, &attempt); err != nil {
			t.Fatal(err)
		}
		if !a2.Equal(accepted) || !d2.Equal(deadline) || l2.After(deadline) || h2 != h || rev != 2 || attempt != 2 {
			t.Fatal("claim renewed immutable acceptance/history")
		}
		if err = f.r.Store.FinishSyntheticMedia(ctx, f.r.Delivery, c1, pin); err == nil {
			t.Fatal("old fence READY")
		}
		mediaWaitDBUTC(t, f.r.Store.DB, deadline)
		if _, err = f.r.Store.RuntimeMediaStagePlan(ctx, f.r.Files, c2); err == nil {
			t.Fatal("deadline plan")
		}
		if _, err = f.r.Store.ClaimSyntheticMedia(ctx, f.r.Delivery); !errors.Is(err, store.ErrNoTask) {
			t.Fatal("deadline settlement", err)
		}
		var state string
		if err = f.r.Store.DB.QueryRow("SELECT m.state,m.revision,m.attempt_count,s.update_seq FROM delivery_media m JOIN delivery_streams s USING(tenant_id,product_id,world_instance_id) WHERE m.asset_id=?", p1.Asset).Scan(&state, &rev, &attempt, &h2); err != nil || state != "FAILED" || rev != 3 || attempt != 2 || h2 != h+1 {
			t.Fatal("absolute deadline terminal", state, rev, attempt, h2, err)
		}
		if _, err = f.r.Store.ClaimSyntheticMedia(ctx, f.r.Delivery); !errors.Is(err, store.ErrNoTask) {
			t.Fatal("FAILED regenerated", err)
		}
		if e := f.r.Files.CompleteMediaFinalEvidence(ctx, c1, pin); e != nil {
			t.Fatal("return original stage before maintenance", e)
		}
		failedView, e := f.r.Delivery.GetTurn(f.client, c1Task(t, f, p1.Asset))
		if e != nil || len(failedView.ApprovedPackets) != 1 || len(failedView.ApprovedPackets[0].Media) != 1 {
			t.Fatal("current-qualified FAILED positive", e)
		}
		var descriptor map[string]any
		if json.Unmarshal(failedView.ApprovedPackets[0].Media[0], &descriptor) != nil || descriptor["media_state"] != "FAILED" {
			t.Fatal("FAILED shape not reached")
		}
		history, e := f.r.Delivery.GetEventsPage(f.client, f.world.WorldInstanceID, "", 100)
		if e != nil {
			t.Fatal(e)
		}
		retainSessionScanMaterial(t, history.NextCursor)
		if removed, e := f.r.Delivery.CleanupSyntheticMedia(context.Background(), p1.Asset); e != nil || !removed {
			t.Fatal("FAILED two accepted slots, actual first file", removed, e)
		}
		intentView, e := f.r.Delivery.GetTurn(f.client, c1Task(t, f, p1.Asset))
		if e != nil || !reflect.DeepEqual(intentView, failedView) {
			t.Fatal("terminal intent changed current text/shape", e)
		}
		eof, e := f.r.Delivery.GetEventsPage(f.client, f.world.WorldInstanceID, history.NextCursor, 100)
		if e != nil || eof.NextCursor != history.NextCursor || len(eof.Deliveries) != 0 || len(eof.Events) != 0 {
			t.Fatal("intent changed history cursor/H", e)
		}
		var intent string
		if e = f.r.Store.DB.QueryRow("SELECT lease_owner FROM delivery_media WHERE asset_id=?", p1.Asset).Scan(&intent); e != nil {
			t.Fatal(e)
		}
		for _, fault := range []string{"fake_marker", "nonNULL_marker", "wrong_slot", "wrong_generation"} {
			t.Run("FAILED_current_shape_"+fault, func(t *testing.T) {
				q := "UPDATE delivery_media SET lease_owner=? WHERE asset_id=?"
				value := any(fmt.Sprintf("%064x", 5))
				switch fault {
				case "nonNULL_marker":
					q = "UPDATE delivery_media SET lease_expires_at=? WHERE asset_id=?"
					value = l2
				case "wrong_slot":
					q = "UPDATE delivery_media SET runtime_file_ref=? WHERE asset_id=?"
					value = fmt.Sprintf("%064x", 6)
				case "wrong_generation":
					q = "UPDATE delivery_media SET maintenance_generation=? WHERE asset_id=?"
					value = 99999
				}
				r, e := f.r.Store.DB.Exec(q, value, p1.Asset)
				if e != nil {
					t.Fatal(e)
				}
				n, e := r.RowsAffected()
				if e != nil || n != 1 {
					t.Fatal("shape fault not applied", n, e)
				}
				_, readErr := f.r.Delivery.GetTurn(f.client, c1Task(t, f, p1.Asset))
				var generation uint64
				if e = f.r.Store.DB.QueryRow("SELECT generation FROM memory_installation").Scan(&generation); e != nil {
					t.Fatal(e)
				}
				r, e = f.r.Store.DB.Exec("UPDATE delivery_media SET lease_owner=?,lease_expires_at=NULL,runtime_file_ref=NULL,maintenance_generation=? WHERE asset_id=?", intent, generation, p1.Asset)
				if e != nil {
					t.Fatal(e)
				}
				n, e = r.RowsAffected()
				if e != nil || n != 1 {
					t.Fatal("shape restore", n, e)
				}
				if readErr == nil {
					t.Fatal("actual current FAILED shape accepted", fault)
				}
			})
		}
		if _, e = f.r.Store.ApplyPlayerSelfConsent(f.client, "failed-to-withdrawn", json.RawMessage(`{"command":"REVOKE","expected_revision":"1","policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`)); e != nil {
			t.Fatal(e)
		}
		if changed, e := f.r.Delivery.ReconcileSyntheticMedia(context.Background(), p1.Asset); e != nil || !changed {
			t.Fatal("FAILED to WITHDRAWN", changed, e)
		}
		var afterIntent string
		var null sql.NullTime
		if e = f.r.Store.DB.QueryRow("SELECT state,revision,lease_owner,lease_expires_at FROM delivery_media WHERE asset_id=?", p1.Asset).Scan(&state, &rev, &afterIntent, &null); e != nil || state != "WITHDRAWN" || rev != 4 || afterIntent != intent || null.Valid {
			t.Fatal("stable permanent intent", state, rev, e)
		}
	})
	for _, kind := range []string{"commit_ack_lost", "rollback_requires_absence", "commit_ack_lost_then_retired", "original_transaction_still_active"} {
		t.Run(kind, func(t *testing.T) {
			f, _ := deliveryFragmentFixture(t, false)
			ctx, done, err := f.r.Local.Participant.EnterDelivery(context.Background(), f.r.Store)
			if err != nil {
				t.Fatal(err)
			}
			defer done()
			c, err := f.r.Store.ClaimSyntheticMedia(ctx, f.r.Delivery)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			stage, err := f.r.Files.StageRuntimeTone(ctx, c)
			if err != nil {
				t.Fatal(err)
			}
			defer stage.Close()
			ev, err := f.r.Files.SealRuntimeMediaStage(ctx, c, stage)
			if err != nil {
				t.Fatal(err)
			}
			pin, err := f.r.Files.AcquireMediaFinalEvidence(c, ev)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if e := f.r.Files.CompleteMediaFinalEvidence(context.WithoutCancel(ctx), c, pin); e != nil {
					t.Error(e)
				}
			}()
			reached := false
			if kind != "rollback_requires_absence" && kind != "original_transaction_still_active" {
				f.r.Store.Commit = func(_ context.Context, tx *sql.Tx) error {
					reached = true
					if e := tx.Commit(); e != nil {
						return e
					}
					return errors.New("synthetic lost media ACK")
				}
			} else {
				f.r.Store.BeforeCommit = func(context.Context, *sql.Tx) error { reached = true; return errors.New("synthetic media rollback") }
			}
			if kind == "original_transaction_still_active" {
				entered, resume := make(chan struct{}), make(chan struct{})
				ended := make(chan error, 1)
				f.r.Store.BeforeCommit = func(context.Context, *sql.Tx) error {
					reached = true
					close(entered)
					<-resume
					return errors.New("explicit original rollback after active observation")
				}
				go func() { ended <- f.r.Store.FinishSyntheticMedia(ctx, f.r.Delivery, c, pin) }()
				<-entered
				d, e := f.r.Store.MediaFinalOutcome(ctx, f.r.Files, c)
				close(resume)
				err = <-ended
				if d != store.MediaFinalUnknown || e != nil {
					t.Fatal("unended original inferred committed", d, e)
				}
			} else {
				err = f.r.Store.FinishSyntheticMedia(ctx, f.r.Delivery, c, pin)
			}
			f.r.Store.Commit = nil
			f.r.Store.BeforeCommit = nil
			if err == nil || !reached {
				t.Fatal("real final interruption", reached, err)
			}
			if kind == "commit_ack_lost_then_retired" {
				var e error
				var asset string
				if e = f.r.Store.DB.QueryRow("SELECT asset_id FROM delivery_media WHERE world_instance_id=?", f.world.WorldInstanceID).Scan(&asset); e != nil {
					t.Fatal(e)
				}
				if _, e = f.r.Store.ApplyPlayerSelfConsent(f.client, "unknown-ready-revoke", json.RawMessage(`{"command":"REVOKE","expected_revision":"1","policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`)); e != nil {
					t.Fatal(e)
				}
				if changed, e := f.r.Delivery.ReconcileSyntheticMedia(context.Background(), asset); e != nil || !changed {
					t.Fatal("actual unknown READY retirement", changed, e)
				}
				purge, e := f.r.Store.ClaimRuntimeMediaPurge(ctx, f.r.Delivery, f.r.Files, asset)
				if e != nil {
					t.Fatal("retained terminal chain intent", e)
				}
				defer purge.Close()
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				if d, e := f.r.Store.MediaFinalOutcome(cancelled, f.r.Files, c); d != store.MediaFinalUnknown || e == nil {
					t.Fatal("cancelled outcome", d, e)
				}
				var task string
				if e = f.r.Store.DB.QueryRow("SELECT task_ref FROM delivery_updates WHERE packet_ref=(SELECT packet_id FROM delivery_media WHERE asset_id=?) AND kind='MEDIA' AND delivery_state='READY'", asset).Scan(&task); e != nil {
					t.Fatal(e)
				}
				r, e := f.r.Store.DB.Exec("UPDATE delivery_updates SET resource_revision=99 WHERE task_ref=? AND kind='MEDIA' AND delivery_state='READY'", task)
				if e != nil {
					t.Fatal(e)
				}
				if n, _ := r.RowsAffected(); n != 1 {
					t.Fatal("READY proof injection")
				}
				d, e := f.r.Store.MediaFinalOutcome(ctx, f.r.Files, c)
				if _, er := f.r.Store.DB.Exec("UPDATE delivery_updates SET resource_revision=3 WHERE task_ref=? AND kind='MEDIA' AND delivery_state='READY'", task); er != nil {
					t.Fatal(er)
				}
				if d != store.MediaFinalUnknown || e == nil {
					t.Fatal("bad original READY became commit", d, e)
				}
			}
			if kind == "rollback_requires_absence" || kind == "original_transaction_still_active" {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				d, e := f.r.Store.MediaFinalOutcome(cancelled, f.r.Files, c)
				if d != store.MediaFinalUnknown || e == nil {
					t.Fatal("rollback alone without successful exact absence read cannot confirm", d, e)
				}
			}
			d, err := f.r.Store.MediaFinalOutcome(ctx, f.r.Files, c)
			want := store.MediaFinalCommitted
			if kind == "rollback_requires_absence" || kind == "original_transaction_still_active" {
				want = store.MediaFinalNotCommitted
			}
			if err != nil || d != want {
				t.Fatal("original transaction reconciliation", d, want, err)
			}
		})
	}
}
func mediaWaitDBUTC(t *testing.T, db *sql.DB, target time.Time) {
	t.Helper()
	var micros int64
	if err := db.QueryRow("SELECT TIMESTAMPDIFF(MICROSECOND,UTC_TIMESTAMP(6),?)", target).Scan(&micros); err != nil {
		t.Fatal(err)
	}
	if micros < 0 || micros > 60000000 {
		t.Fatal("natural timing precondition", micros)
	}
	t.Logf("natural media DBUTC wait microseconds=%d", micros)
	time.Sleep(time.Duration(micros)*time.Microsecond + 10*time.Millisecond)
}

func c1Task(t *testing.T, f integrationFixFixture, asset string) string {
	t.Helper()
	var task string
	if e := f.r.Store.DB.QueryRow("SELECT c.task_id FROM delivery_media m JOIN harness_turn_commits c ON c.tenant_id=m.tenant_id AND c.product_id=m.product_id AND c.result_id=m.original_result_ref WHERE m.asset_id=?", asset).Scan(&task); e != nil {
		t.Fatal(e)
	}
	return task
}

// Only this owning synthetic file is displaced. The original raw body and
// native object are restored; a metadata observation is not a historical hash.
func mediaOriginalMetadata(t *testing.T, kind string) {
	f, _ := deliveryFragmentFixture(t, false)
	ctx, done, err := f.r.Local.Participant.EnterDelivery(context.Background(), f.r.Store)
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	c, err := f.r.Store.ClaimSyntheticMedia(ctx, f.r.Delivery)
	if err != nil {
		t.Fatal("owning claim before metadata fault", err)
	}
	defer c.Close()
	plan, err := f.r.Store.RuntimeMediaStagePlan(ctx, f.r.Files, c)
	if err != nil {
		t.Fatal("closed current plan before metadata fault", err)
	}
	var original string
	var info os.FileInfo
	for _, ext := range []string{".part", ".blob"} {
		p := filepath.Join(f.r.Files.RootPath, "demo-local", "coffee", plan.OriginalStorage+ext)
		if v, e := os.Lstat(p); e == nil {
			original, info = p, v
			break
		}
	}
	if info == nil || !info.Mode().IsRegular() || info.Size() <= 0 {
		t.Fatal("regular original prerequisite")
	}
	late := kind == "replacement" || kind == "size_between_observations"
	var stage *files.RuntimeMediaStage
	if late {
		stage, err = f.r.Files.StageRuntimeTone(ctx, c)
		if err != nil {
			t.Fatal("actual first metadata observation", err)
		}
		defer stage.Close()
	}
	if kind == "size_between_observations" {
		file, e := os.OpenFile(original, os.O_WRONLY|os.O_APPEND, 0)
		if e != nil {
			t.Fatal(e)
		}
		n, we := file.Write([]byte{0})
		ce := file.Close()
		if we != nil || ce != nil || n != 1 {
			t.Fatal("size injection", n, we, ce)
		}
		defer func() {
			if e := os.Truncate(original, info.Size()); e != nil {
				t.Error(e)
			}
		}()
	} else {
		saved := original + ".owned-original"
		if err = os.Rename(original, saved); err != nil {
			t.Fatal(err)
		}
		created := false
		defer func() {
			if created {
				if e := os.Remove(original); e != nil {
					t.Error(e)
					return
				}
			}
			if e := os.Rename(saved, original); e != nil {
				t.Error(e)
			}
		}()
		switch kind {
		case "nonregular":
			err = os.Mkdir(original, 0700)
		case "alias":
			err = os.Symlink(saved, original)
			if err != nil {
				t.Skipf("PLATFORM_CAPABILITY_NOT_RUN: real file symlink creation: %v", err)
			}
		default:
			var file *os.File
			file, err = os.OpenFile(original, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err == nil {
				size := int64(0)
				if kind == "oversize" {
					size = int64(files.MaxFileBytes) + 1
				}
				if kind == "replacement" {
					size = info.Size()
				}
				err = file.Truncate(size)
				if e := file.Close(); err == nil {
					err = e
				}
			}
		}
		if err != nil {
			t.Fatal("metadata fault setup", err)
		}
		created = true
	}
	if late {
		_, err = f.r.Files.SealRuntimeMediaStage(ctx, c, stage)
	} else {
		_, err = f.r.Files.StageRuntimeTone(ctx, c)
	}
	if !errors.Is(err, files.ErrNotFound) {
		t.Fatal("Files metadata gate must be reached", kind, err)
	}
	var state string
	var revision uint64
	if err = f.r.Store.DB.QueryRow("SELECT state,revision FROM delivery_media WHERE asset_id=?", plan.Asset).Scan(&state, &revision); err != nil || state != "PENDING" || revision != 2 {
		t.Fatal("metadata failure changed READY state", state, revision, err)
	}
}
func mediaOwningHappy(t *testing.T) {
	f, task := deliveryFragmentFixture(t, false)
	view, err := f.r.Delivery.GetTurn(f.client, task)
	if err != nil || len(view.ApprovedPackets) != 1 || len(view.ApprovedPackets[0].Media) != 0 {
		t.Fatal("lawful committed packet prerequisite", err)
	}
	var before uint64
	if err = f.r.Store.DB.QueryRow("SELECT update_seq FROM delivery_streams WHERE world_instance_id=?", f.world.WorldInstanceID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	producer, ok := any(f.r.Delivery).(interface {
		ProduceOneSyntheticMedia(context.Context) (bool, error)
	})
	if !ok {
		t.Fatal("CAUSAL_RED: lawful committed packet reached missing owning synthetic-media port")
	}
	worked, err := producer.ProduceOneSyntheticMedia(context.Background())
	if err != nil || !worked {
		t.Fatal("owning synthetic production", worked, err)
	}
	var state, mime string
	var size, revision, attempts, fence, h uint64
	if err = f.r.Store.DB.QueryRow("SELECT m.state,m.mime_type,m.byte_length,m.revision,m.attempt_count,m.fence,s.update_seq FROM delivery_media m JOIN delivery_streams s USING(tenant_id,product_id,world_instance_id) WHERE m.packet_id=?", view.ApprovedPackets[0].PacketID).Scan(&state, &mime, &size, &revision, &attempts, &fence, &h); err != nil {
		t.Fatal(err)
	}
	if state != "READY" || mime != "audio/wav" || size != 160044 || revision != 3 || attempts != 1 || fence != 1 || h != before+2 {
		t.Fatalf("actual fixed media lifecycle: state=%s mime=%s bytes=%d rev=%d attempts=%d fence=%d hdelta=%d", state, mime, size, revision, attempts, fence, h-before)
	}
	var storage, asset, hash string
	if err = f.r.Store.DB.QueryRow("SELECT runtime_file_ref,asset_id,LOWER(HEX(content_hash)) FROM delivery_media WHERE packet_id=?", view.ApprovedPackets[0].PacketID).Scan(&storage, &asset, &hash); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(f.r.Files.RootPath, "demo-local", "coffee", "runtime-media", storage+".wav"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	if len(body) != 160044 || string(body[:4]) != "RIFF" || string(body[8:12]) != "WAVE" || binary.LittleEndian.Uint32(body[24:28]) != 16000 || binary.LittleEndian.Uint16(body[22:24]) != 1 || binary.LittleEndian.Uint16(body[34:36]) != 16 || hex.EncodeToString(sum[:]) != hash {
		t.Fatal("actual PCM header/size/hash")
	}
	operator, _, err := f.r.Store.AuthenticateOperator(context.Background(), "Bearer "+runtimeToken)
	if err != nil {
		t.Fatal("real installed operator", err)
	}
	if _, err = f.r.Files.Get(operator, domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}, asset); err == nil {
		t.Fatal("runtime media escaped as ordinary Admin file")
	}
	_, err = f.r.Files.Stage(operator, domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}, bytes.NewReader(body), generated.AssetUploadMetadata{ProductID: "coffee", Name: "tone.wav", DeclaredType: "audio/wav", ExpectedSize: "160044", ExpectedSha256: hash})
	if err == nil {
		t.Fatal("ordinary upload accepted new WAV MIME")
	}
	worked, err = producer.ProduceOneSyntheticMedia(context.Background())
	if err != nil || worked {
		t.Fatal("READY cannot regenerate", worked, err)
	}
	var notices, calls, commits int
	if err = f.r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM integration_notifications WHERE result_ref=?),calls_used,(SELECT COUNT(*) FROM harness_turn_commits WHERE task_id=?) FROM tasks WHERE task_id=?", view.ApprovedPackets[0].PacketID, task, task).Scan(&notices, &calls, &commits); err != nil || notices != 1 || calls != 1 || commits != 1 {
		t.Fatal("media cannot repeat world/provider/initial notice", notices, calls, commits, err)
	}
}

// These use genuine owning claims; only the named fault is injected. No test
// manufactures a receipt, authorization interface, current principal, or lease.
func mediaOwningFailure(t *testing.T, kind string) {
	f, task := deliveryFragmentFixture(t, false)
	ctx, done, e := f.r.Local.Participant.EnterDelivery(context.Background(), f.r.Store)
	if e != nil {
		t.Fatal(e)
	}
	defer done()
	c, e := f.r.Store.ClaimSyntheticMedia(ctx, f.r.Delivery)
	if e != nil {
		t.Fatal("legitimate claim prerequisite", e)
	}
	defer c.Close()
	plan, e := f.r.Store.RuntimeMediaStagePlan(ctx, f.r.Files, c)
	if e != nil {
		t.Fatal("legitimate closed plan", e)
	}
	if kind == "zero_copy_foreign" {
		if _, e = f.r.Store.RuntimeMediaStagePlan(ctx, f.r.Files, &store.RuntimeMediaClaim{}); e == nil {
			t.Fatal("zero claim")
		}
		copied := new(store.RuntimeMediaClaim)
		reflect.ValueOf(copied).Elem().Set(reflect.ValueOf(c).Elem())
		if _, e = f.r.Store.RuntimeMediaStagePlan(ctx, f.r.Files, copied); e == nil {
			t.Fatal("copied claim")
		}
		foreign := *f.r.Files
		if _, e = f.r.Store.RuntimeMediaStagePlan(ctx, &foreign, c); e == nil {
			t.Fatal("same-Store foreign Files")
		}
	}
	if kind == "missing_original" {
		var original string
		for _, ext := range []string{".part", ".blob"} {
			p := filepath.Join(f.r.Files.RootPath, "demo-local", "coffee", plan.OriginalStorage+ext)
			if _, e = os.Stat(p); e == nil {
				original = p
				break
			}
		}
		if original == "" {
			t.Fatal("positive original presence")
		}
		saved := original + ".test-retained"
		if e = os.Rename(original, saved); e != nil {
			t.Fatal("owned absence injection", e)
		}
		defer func() {
			if e := os.Rename(saved, original); e != nil {
				t.Error(e)
			}
		}()
		if _, e = f.r.Files.StageRuntimeTone(ctx, c); e == nil {
			t.Fatal("missing original accepted")
		}
		return
	}
	stage, e := f.r.Files.StageRuntimeTone(ctx, c)
	if e != nil {
		t.Fatal("actual stage prerequisite", e)
	}
	defer stage.Close()
	if kind == "seal_loss" {
		if e = stage.Close(); e != nil {
			t.Fatal(e)
		}
		if _, e = f.r.Files.SealRuntimeMediaStage(ctx, c, stage); e == nil {
			t.Fatal("closed native stage sealed")
		}
		return
	}
	if kind == "zero_copy_foreign" {
		copyStage := new(files.RuntimeMediaStage)
		reflect.ValueOf(copyStage).Elem().Set(reflect.ValueOf(stage).Elem())
		if _, e = f.r.Files.SealRuntimeMediaStage(ctx, c, copyStage); e == nil {
			t.Fatal("copied stage")
		}
	}
	ev, e := f.r.Files.SealRuntimeMediaStage(ctx, c, stage)
	if e != nil {
		t.Fatal("actual sealed evidence prerequisite", e)
	}
	if kind == "zero_copy_foreign" {
		copyEvidence := *ev
		if _, e = f.r.Files.AcquireMediaFinalEvidence(c, &copyEvidence); e == nil {
			t.Fatal("copied evidence")
		}
	}
	pin, e := f.r.Files.AcquireMediaFinalEvidence(c, ev)
	if e != nil {
		t.Fatal("actual pin prerequisite", e)
	}
	defer func() {
		if e := f.r.Files.CompleteMediaFinalEvidence(context.WithoutCancel(ctx), c, pin); e != nil {
			t.Error(e)
		}
	}()
	if kind == "zero_copy_foreign" {
		copyPin := *pin
		if e = f.r.Store.FinishSyntheticMedia(ctx, f.r.Delivery, c, &copyPin); e == nil {
			t.Fatal("copied pin")
		}
		if e = f.r.Store.FinishSyntheticMedia(ctx, f.r.Delivery, c, pin); e != nil {
			t.Fatal("original positive pin", e)
		}
		return
	}
	var beforeH uint64
	if e = f.r.Store.DB.QueryRow("SELECT update_seq FROM delivery_streams WHERE world_instance_id=?", f.world.WorldInstanceID).Scan(&beforeH); e != nil {
		t.Fatal(e)
	}
	reached := false
	f.r.Store.BeforeCommit = func(ctx context.Context, tx *sql.Tx) error {
		reached = true
		switch kind {
		case "final_close":
			return stage.Close()
		case "final_row_drift":
			r, e := tx.ExecContext(ctx, "UPDATE delivery_media SET media_id='different-media' WHERE asset_id=?", plan.Asset)
			if e != nil {
				return e
			}
			n, e := r.RowsAffected()
			if e != nil {
				return e
			}
			if n != 1 {
				t.Fatal("fault row not reached")
			}
		case "final_H_drift":
			r, e := tx.ExecContext(ctx, "UPDATE delivery_streams SET update_seq=update_seq+1 WHERE world_instance_id=?", f.world.WorldInstanceID)
			if e != nil {
				return e
			}
			n, e := r.RowsAffected()
			if e != nil {
				return e
			}
			if n != 1 {
				t.Fatal("fault H not reached")
			}
		}
		return nil
	}
	e = f.r.Store.FinishSyntheticMedia(ctx, f.r.Delivery, c, pin)
	f.r.Store.BeforeCommit = nil
	if e == nil || !reached {
		t.Fatal("after-hook current refusal", kind, reached, e)
	}
	var state string
	var revision, h uint64
	if e = f.r.Store.DB.QueryRow("SELECT m.state,m.revision,s.update_seq FROM delivery_media m JOIN delivery_streams s USING(tenant_id,product_id,world_instance_id) WHERE m.asset_id=?", plan.Asset).Scan(&state, &revision, &h); e != nil || state != "PENDING" || revision != 2 || h != beforeH {
		t.Fatal("whole READY/update rollback", state, revision, h, e)
	}
	var commits int
	if e = f.r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_turn_commits WHERE task_id=?", task).Scan(&commits); e != nil || commits != 1 {
		t.Fatal("original COMMIT changed", e)
	}
}

func TestIntegrationMediaCloseAndCleanup(t *testing.T) {
	t.Run("retained_proof_and_terminal_negative_matrix", mediaRetirementMatrix)
	t.Run("old_producer_reservation_recheck", mediaLateWriterRetirement)
	t.Run("owning_source_withdrawal_receipt", func(t *testing.T) {
		f, _ := deliveryFragmentFixture(t, false)
		if ok, e := f.r.Delivery.ProduceOneSyntheticMedia(context.Background()); e != nil || !ok {
			t.Fatal(e)
		}
		var asset, release string
		if e := f.r.Store.DB.QueryRow("SELECT m.asset_id,w.release_id FROM delivery_media m JOIN world_controls w USING(tenant_id,product_id,world_instance_id) WHERE m.world_instance_id=?", f.world.WorldInstanceID).Scan(&asset, &release); e != nil {
			t.Fatal(e)
		}
		op := runtimeContext(t, f.r)
		pub, e := f.r.Content.GetRelease(op, f.r.Auth.Scope(), release)
		if e != nil {
			t.Fatal(e)
		}
		withdrawalAssert(t, f.r, op, pub, "media-source-withdrawal")
		if changed, e := f.r.Delivery.ReconcileSyntheticMedia(context.Background(), asset); e != nil || !changed {
			t.Fatal("real governance witness", changed, e)
		}
		if removed, e := f.r.Delivery.CleanupSyntheticMedia(context.Background(), asset); e != nil || !removed {
			t.Fatal("retained source cleanup", removed, e)
		}
	})
	t.Run("actual_REVOKE_terminal_intent_and_indexed_cleanup", func(t *testing.T) {
		f, task := deliveryFragmentFixture(t, false)
		if worked, e := f.r.Delivery.ProduceOneSyntheticMedia(context.Background()); e != nil || !worked {
			t.Fatal("real READY producer prerequisite", worked, e)
		}
		var asset, storage, originalHistory string
		var beforeH uint64
		if e := f.r.Store.DB.QueryRow("SELECT asset_id,runtime_file_ref FROM delivery_media WHERE world_instance_id=? AND state='READY'", f.world.WorldInstanceID).Scan(&asset, &storage); e != nil {
			t.Fatal(e)
		}
		if e := f.r.Store.DB.QueryRow("SELECT CAST(JSON_ARRAY(update_seq,business_update_id,delivery_state,resource_revision,packet_ref,audience_refs,lineage_refs) AS CHAR) FROM delivery_updates WHERE task_ref=? AND kind='MEDIA' AND delivery_state='READY'", task).Scan(&originalHistory); e != nil {
			t.Fatal(e)
		}
		if e := f.r.Store.DB.QueryRow("SELECT update_seq FROM delivery_streams WHERE world_instance_id=?", f.world.WorldInstanceID).Scan(&beforeH); e != nil {
			t.Fatal(e)
		}
		if _, e := f.r.Store.ApplyPlayerSelfConsent(f.client, "media-retirement-self-revoke", json.RawMessage(`{"command":"REVOKE","expected_revision":"1","policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`)); e != nil {
			t.Fatal("actual persistent REVOKE witness prerequisite", e)
		}
		port, ok := any(f.r.Delivery).(interface {
			ReconcileSyntheticMedia(context.Context, string) (bool, error)
			CleanupSyntheticMedia(context.Context, string) (bool, error)
		})
		if !ok {
			t.Fatal("CAUSAL_RED: actual READY and owning self REVOKE reached missing closed retirement/purge ports")
		}
		changed, e := port.ReconcileSyntheticMedia(context.Background(), asset)
		if e != nil || !changed {
			t.Fatal("owning once withdrawal", changed, e)
		}
		changed, e = port.ReconcileSyntheticMedia(context.Background(), asset)
		if e != nil || changed {
			t.Fatal("same original retirement replay", changed, e)
		}
		var state, history string
		var revision, h uint64
		if e = f.r.Store.DB.QueryRow("SELECT state,revision FROM delivery_media WHERE asset_id=?", asset).Scan(&state, &revision); e != nil {
			t.Fatal(e)
		}
		if e = f.r.Store.DB.QueryRow("SELECT update_seq FROM delivery_streams WHERE world_instance_id=?", f.world.WorldInstanceID).Scan(&h); e != nil {
			t.Fatal(e)
		}
		if e = f.r.Store.DB.QueryRow("SELECT CAST(JSON_ARRAY(update_seq,business_update_id,delivery_state,resource_revision,packet_ref,audience_refs,lineage_refs) AS CHAR) FROM delivery_updates WHERE task_ref=? AND kind='MEDIA' AND delivery_state='READY'", task).Scan(&history); e != nil {
			t.Fatal(e)
		}
		if state != "WITHDRAWN" || revision != 4 || h != beforeH+1 || history != originalHistory {
			t.Fatal("once transition/history", state, revision, h, beforeH)
		}
		if _, e = f.r.Delivery.ReadRuntimeMedia(f.client, asset, delivery.MediaRange{}); e == nil {
			t.Fatal("revoked playback allowed")
		}
		file := filepath.Join(f.r.Files.RootPath, "demo-local", "coffee", "runtime-media", storage+".wav")
		if _, e = os.Stat(file); e != nil {
			t.Fatal("actual indexed file prerequisite", e)
		}
		removed, e := port.CleanupSyntheticMedia(context.Background(), asset)
		if e != nil || !removed {
			t.Fatal("actual indexed terminal cleanup", removed, e)
		}
		if _, e = os.Lstat(file); !os.IsNotExist(e) {
			t.Fatal("indexed own file remains", e)
		}
		var marker string
		var lease sql.NullTime
		if e = f.r.Store.DB.QueryRow("SELECT lease_owner,lease_expires_at FROM delivery_media WHERE asset_id=?", asset).Scan(&marker, &lease); e != nil || len(marker) != 64 || lease.Valid {
			t.Fatal("committed exact intent/NULL prerequisite", e)
		}
		removed, e = port.CleanupSyntheticMedia(context.Background(), asset)
		if e != nil || removed {
			t.Fatal("same-intent restartable absence", removed, e)
		}
	})
}

func mediaLateWriterRetirement(t *testing.T) {
	f, _ := deliveryFragmentFixture(t, false)
	parent, done, e := f.r.Local.Participant.EnterDelivery(context.Background(), f.r.Store)
	if e != nil {
		t.Fatal(e)
	}
	defer done()
	c, e := f.r.Store.ClaimSyntheticMedia(parent, f.r.Delivery)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	plan, e := f.r.Store.RuntimeMediaStagePlan(parent, f.r.Files, c)
	if e != nil {
		t.Fatal(e)
	}
	reservation, e := localstate.Open(filepath.Join(f.r.Files.RootPath, "demo-local", "coffee", "runtime-media-locks", plan.StageStorage), true)
	if e != nil {
		t.Fatal(e)
	}
	bounded, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	unlock, e := reservation.Lock(bounded, "operations.lock")
	if e != nil {
		t.Fatal(e)
	}
	held := true
	defer func() {
		if held {
			unlock()
		}
	}()
	reached := make(chan struct{})
	var traced atomic.Bool
	f.r.Store.Trace = func(k store.LockKey) {
		if k.Rank == 110 && traced.CompareAndSwap(false, true) {
			close(reached)
		}
	}
	type result struct {
		stage *files.RuntimeMediaStage
		err   error
	}
	finished := make(chan result, 1)
	go func() { v, e := f.r.Files.StageRuntimeTone(bounded, c); finished <- result{v, e} }()
	select {
	case <-reached:
	case <-bounded.Done():
		t.Fatal("old StagePlan did not reach original SQL", bounded.Err())
	}
	for f.r.Store.DB.Stats().InUse != 0 {
		select {
		case <-bounded.Done():
			t.Fatal(bounded.Err())
		case <-time.After(time.Millisecond):
		}
	}
	f.r.Store.Trace = nil
	select {
	case r := <-finished:
		if r.stage != nil {
			r.stage.Close()
		}
		t.Fatal("held reservation not respected", r.err)
	default:
	}
	if _, e = f.r.Store.ApplyPlayerSelfConsent(f.client, "late-stage-revoke", json.RawMessage(`{"command":"REVOKE","expected_revision":"1","policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`)); e != nil {
		t.Fatal(e)
	}
	if changed, e := f.r.Delivery.ReconcileSyntheticMedia(context.Background(), plan.Asset); e != nil || !changed {
		t.Fatal(e)
	}
	purge, e := f.r.Store.ClaimRuntimeMediaPurge(parent, f.r.Delivery, f.r.Files, plan.Asset)
	if e != nil {
		t.Fatal(e)
	}
	defer purge.Close()
	unlock()
	held = false
	r := <-finished
	if r.stage != nil {
		r.stage.Close()
	}
	if r.err == nil {
		t.Fatal("old StagePlan wrote after terminal intent")
	}
	file := filepath.Join(f.r.Files.RootPath, "demo-local", "coffee", "runtime-media", plan.StageStorage+".wav")
	if _, e = os.Lstat(file); !os.IsNotExist(e) {
		t.Fatal("late stage recreated native file", e)
	}
	if removed, e := f.r.Files.CleanupRuntimeMedia(parent, purge); e != nil || removed {
		t.Fatal("unwritten accepted slot", removed, e)
	}
	t.Log("actual producer blocked on shared native reservation with SQL returned; permanent intent committed before release; second SQL current gate denied before native body IO")
}

func mediaRetirementMatrix(t *testing.T) {
	f, task := deliveryFragmentFixture(t, false)
	if ok, e := f.r.Delivery.ProduceOneSyntheticMedia(context.Background()); e != nil || !ok {
		t.Fatal("READY prerequisite", e)
	}
	var asset, storage string
	if e := f.r.Store.DB.QueryRow("SELECT asset_id,runtime_file_ref FROM delivery_media WHERE world_instance_id=?", f.world.WorldInstanceID).Scan(&asset, &storage); e != nil {
		t.Fatal(e)
	}
	file := filepath.Join(f.r.Files.RootPath, "demo-local", "coffee", "runtime-media", storage+".wav")
	snapshot := func() string {
		var b string
		e := f.r.Store.DB.QueryRow("SELECT CAST(JSON_ARRAY(m.state,m.revision,m.lease_owner,m.lease_expires_at,m.fence,m.attempt_count,m.runtime_file_ref,HEX(m.content_hash),m.byte_length,m.accepted_at,m.execute_by,m.maintenance_generation,s.update_seq) AS CHAR) FROM delivery_media m JOIN delivery_streams s USING(tenant_id,product_id,world_instance_id) WHERE asset_id=?", asset).Scan(&b)
		if e != nil {
			t.Fatal(e)
		}
		return b
	}
	exec := func(q string, args ...any) {
		r, e := f.r.Store.DB.Exec(q, args...)
		if e != nil {
			t.Fatal("fault/setup SQL", e)
		}
		n, e := r.RowsAffected()
		if e != nil || n != 1 {
			t.Fatal("exact one-row injection", n, e)
		}
	}
	before := snapshot()
	t.Run("READY_and_unknown_locator_never_cleanup", func(t *testing.T) {
		if changed, e := f.r.Delivery.ReconcileSyntheticMedia(context.Background(), asset); e != nil || changed {
			t.Fatal("no persistent witness", changed, e)
		}
		for _, id := range []string{asset, fmt.Sprintf("%064x", 7)} {
			if _, e := f.r.Delivery.CleanupSyntheticMedia(context.Background(), id); e == nil {
				t.Fatal("live/unindexed cleanup")
			}
		}
		if snapshot() != before {
			t.Fatal("live media changed")
		}
		if _, e := os.Stat(file); e != nil {
			t.Fatal(e)
		}
	})
	t.Run("temporary_reader_rejection_is_not_witness", func(t *testing.T) {
		exec("UPDATE integration_principals SET revoked=1 WHERE principal_id=(SELECT principal_ref FROM integration_task_principals WHERE task_id=?)", task)
		if _, e := f.r.Delivery.ReadRuntimeMedia(f.client, asset, delivery.MediaRange{}); e == nil {
			t.Fatal("reader fault prerequisite")
		}
		changed, e := f.r.Delivery.ReconcileSyntheticMedia(context.Background(), asset)
		exec("UPDATE integration_principals SET revoked=0 WHERE principal_id=(SELECT principal_ref FROM integration_task_principals WHERE task_id=?)", task)
		if e != nil || changed || snapshot() != before {
			t.Fatal("reader rejection became retirement", changed, e)
		}
	})
	if _, e := f.r.Store.ApplyPlayerSelfConsent(f.client, "media-matrix-revoke", json.RawMessage(`{"command":"REVOKE","expected_revision":"1","policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`)); e != nil {
		t.Fatal(e)
	}
	t.Run("retained_missing_original_commit_rejected", func(t *testing.T) {
		var old []byte
		if e := f.r.Store.DB.QueryRow("SELECT payload_hash FROM commit_receipts WHERE task_id=? AND operation='HARNESS_TURN_COMMIT'", task).Scan(&old); e != nil {
			t.Fatal(e)
		}
		exec("UPDATE commit_receipts SET payload_hash=? WHERE task_id=? AND operation='HARNESS_TURN_COMMIT'", make([]byte, 32), task)
		_, e := f.r.Delivery.ReconcileSyntheticMedia(context.Background(), asset)
		exec("UPDATE commit_receipts SET payload_hash=? WHERE task_id=? AND operation='HARNESS_TURN_COMMIT'", old, task)
		if e == nil || snapshot() != before {
			t.Fatal("bad original COMMIT accepted", e)
		}
	})
	t.Run("withdraw_afterhook_tuple_rollback", func(t *testing.T) {
		reached := false
		f.r.Store.BeforeCommit = func(ctx context.Context, tx *sql.Tx) error {
			reached = true
			r, e := tx.ExecContext(ctx, "UPDATE delivery_media SET runtime_file_ref=? WHERE asset_id=?", fmt.Sprintf("%064x", 8), asset)
			if e != nil {
				return e
			}
			n, e := r.RowsAffected()
			if e != nil || n != 1 {
				return errors.New("fault not reached")
			}
			return nil
		}
		_, e := f.r.Delivery.ReconcileSyntheticMedia(context.Background(), asset)
		f.r.Store.BeforeCommit = nil
		if !reached || e == nil || snapshot() != before {
			t.Fatal("writer final accepted drift", reached, e)
		}
	})
	t.Run("withdraw_ACK_unknown_original_reentry_no_duplicate", func(t *testing.T) {
		reached := false
		f.r.Store.Commit = func(_ context.Context, tx *sql.Tx) error {
			reached = true
			if e := tx.Commit(); e != nil {
				return e
			}
			return errors.New("synthetic withdrawal ACK loss")
		}
		_, e := f.r.Delivery.ReconcileSyntheticMedia(context.Background(), asset)
		f.r.Store.Commit = nil
		if !reached || !errors.Is(e, store.ErrCommitUnknown) {
			t.Fatal("actual committed lost ACK", e)
		}
		var count int
		if e = f.r.Store.DB.QueryRow("SELECT COUNT(*) FROM delivery_updates WHERE task_ref=? AND kind='MEDIA' AND delivery_state='WITHDRAWN'", task).Scan(&count); e != nil || count != 1 {
			t.Fatal("once original", count, e)
		}
		if changed, e := f.r.Delivery.ReconcileSyntheticMedia(context.Background(), asset); e != nil || changed {
			t.Fatal("new withdrawal identity", changed, e)
		}
	})
	terminal := snapshot()
	for _, fault := range []struct {
		name, q string
		value   any
	}{
		{"fake_marker", "UPDATE delivery_media SET lease_owner=?,lease_expires_at=NULL WHERE asset_id=?", fmt.Sprintf("%064x", 3)},
		{"wrong_slot", "UPDATE delivery_media SET runtime_file_ref=? WHERE asset_id=?", fmt.Sprintf("%064x", 4)},
		{"wrong_generation", "UPDATE delivery_media SET maintenance_generation=? WHERE asset_id=?", 99999},
		{"producer_NULL", "UPDATE delivery_media SET lease_expires_at=? WHERE asset_id=?", nil},
	} {
		t.Run(fault.name, func(t *testing.T) {
			var owner string
			var lease time.Time
			var gen uint64
			if e := f.r.Store.DB.QueryRow("SELECT lease_owner,lease_expires_at,maintenance_generation FROM delivery_media WHERE asset_id=?", asset).Scan(&owner, &lease, &gen); e != nil {
				t.Fatal(e)
			}
			exec(fault.q, fault.value, asset)
			_, e := f.r.Delivery.CleanupSyntheticMedia(context.Background(), asset)
			exec("UPDATE delivery_media SET lease_owner=?,lease_expires_at=?,runtime_file_ref=?,maintenance_generation=? WHERE asset_id=?", owner, lease, storage, gen, asset)
			if e == nil || snapshot() != terminal {
				t.Fatal("terminal shape accepted", e)
			}
			if _, e = os.Stat(file); e != nil {
				t.Fatal("file removed", e)
			}
		})
	}
	t.Run("intent_rollback_and_unknown_preserve_native", func(t *testing.T) {
		reached := false
		f.r.Store.BeforeCommit = func(context.Context, *sql.Tx) error { reached = true; return errors.New("synthetic intent rollback") }
		_, e := f.r.Delivery.CleanupSyntheticMedia(context.Background(), asset)
		f.r.Store.BeforeCommit = nil
		if !reached || e == nil || snapshot() != terminal {
			t.Fatal("rollback intent", e)
		}
		f.r.Store.Commit = func(_ context.Context, tx *sql.Tx) error {
			if e := tx.Commit(); e != nil {
				return e
			}
			return errors.New("synthetic intent ACK loss")
		}
		_, e = f.r.Delivery.CleanupSyntheticMedia(context.Background(), asset)
		f.r.Store.Commit = nil
		if !errors.Is(e, store.ErrCommitUnknown) {
			t.Fatal("unknown intent", e)
		}
		if _, e = os.Stat(file); e != nil {
			t.Fatal("unknown deleted file", e)
		}
	})
	t.Run("opaque_copy_closed_cancel_type_and_unindexed", func(t *testing.T) {
		ctx, done, e := f.r.Local.Participant.EnterDelivery(context.Background(), f.r.Store)
		if e != nil {
			t.Fatal(e)
		}
		defer done()
		claim, e := f.r.Store.ClaimRuntimeMediaPurge(ctx, f.r.Delivery, f.r.Files, asset)
		if e != nil {
			t.Fatal("persisted intent recovery", e)
		}
		defer claim.Close()
		copyClaim := *claim
		if _, e = f.r.Store.RecheckRuntimeMediaPurge(ctx, f.r.Files, &copyClaim); e == nil {
			t.Fatal("copy seal")
		}
		if _, e = f.r.Store.RecheckRuntimeMediaPurge(ctx, nil, claim); e == nil {
			t.Fatal("foreign Files")
		}
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, e = f.r.Files.CleanupRuntimeMedia(cancelled, claim); e == nil {
			t.Fatal("cancelled cleanup")
		}
		saved := file + ".saved"
		if e = os.Rename(file, saved); e != nil {
			t.Fatal(e)
		}
		if e = os.Mkdir(file, 0700); e != nil {
			t.Fatal(e)
		}
		_, e = f.r.Files.CleanupRuntimeMedia(ctx, claim)
		if er := os.Remove(file); er != nil {
			t.Fatal(er)
		}
		if er := os.Rename(saved, file); er != nil {
			t.Fatal(er)
		}
		if e == nil {
			t.Fatal("directory removed")
		}
		if e = claim.Close(); e != nil {
			t.Fatal(e)
		}
		if _, e = f.r.Files.CleanupRuntimeMedia(ctx, claim); e == nil {
			t.Fatal("closed claim")
		}
		foreign := filepath.Join(filepath.Dir(file), fmt.Sprintf("%064x.wav", 9))
		body := []byte("unindexed owned fixture")
		if e = os.WriteFile(foreign, body, 0600); e != nil {
			t.Fatal(e)
		}
		if removed, e := f.r.Delivery.CleanupSyntheticMedia(context.Background(), asset); e != nil || !removed {
			t.Fatal("original intent recovery", removed, e)
		}
		got, e := os.ReadFile(foreign)
		if e != nil || !bytes.Equal(got, body) {
			t.Fatal("unindexed file touched", e)
		}
	})
}
