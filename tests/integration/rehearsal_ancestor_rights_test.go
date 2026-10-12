package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/harness"
	store "loreviva.local/loreviva/internal/store/mysql"
)

func TestRehearsalAncestorRightsAtExtraSendAndHistory(t *testing.T) {
	ctx := context.Background()
	r, draft, assets, req := physicalRehearsalDraft(t)
	scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
	// The second actual uploaded asset is an ancestor of the first source.
	// Both allowed audience policies are represented: direct EDITORS, ancestor READERS.
	if _, e := r.Store.DB.Exec("UPDATE assets SET document=JSON_SET(CONVERT(document USING utf8mb4),'$.audience_policy','PRODUCT_READERS') WHERE asset_id=?", assets[1]); e != nil {
		t.Fatal(e)
	}
	var original []byte
	if e := r.Store.DB.QueryRow("SELECT document FROM assets WHERE asset_id=?", assets[1]).Scan(&original); e != nil {
		t.Fatal(e)
	}
	worker := "ancestor-rights"
	root, e := r.Store.AcceptDraftRehearsal(ctx, scope, draft, worker, req)
	if e != nil {
		t.Fatal(e)
	}
	rl, e := r.Store.ClaimRegistered(ctx, worker, time.Now(), time.Minute, []store.RegisteredOperation{{Kind: "ADMIN", Operation: "REHEARSAL", TaskID: string(root.TaskID)}})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.InitializeDraftRehearsal(ctx, rl.TaskLease, worker); e != nil {
		t.Fatal(e)
	}
	cl, e := r.Store.ClaimRehearsalChild(ctx, rl.TaskLease, worker)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.BindRehearsalChildContext(ctx, rl.TaskLease, cl.TaskLease, worker); e != nil {
		t.Fatal(e)
	}
	childCtx, close, e := r.Store.EnterRehearsalChildExecution(ctx, rl.TaskLease, cl.TaskLease, worker)
	if e != nil {
		t.Fatal(e)
	}
	defer close()
	d, e := r.Store.PrepareRehearsalTurn(childCtx, rl.TaskLease, cl.TaskLease, worker)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.AuthorizeDispatch(childCtx, d); e != nil {
		t.Fatal(e)
	}
	d.CommandKey = content.Identity(cl.TaskID, "send")
	d.CommitKey = d.CommandKey
	sent, e := r.Store.StartSend(childCtx, d, d.AttemptID)
	if e != nil {
		t.Fatal(e)
	}
	r.Worker.Turns.Fault = harness.MockMalformedOriginalResult
	if e = r.Worker.Turns.Generate(childCtx, sent.OriginalSend()); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.ObserveOriginalReply(childCtx, sent.OriginalSend()); e != nil {
		t.Fatal(e)
	}
	if e = r.Store.ValidateOriginalTurn(childCtx, cl.TaskLease, worker); e != nil {
		t.Fatal(e)
	}
	d, e = r.Store.PrepareTurnRecovery(childCtx, cl.TaskLease, worker)
	if e != nil {
		t.Fatal(e)
	}
	counts := func() string {
		var out string
		if e := r.Store.DB.QueryRow("SELECT CONCAT((SELECT COUNT(*) FROM attempts),':',(SELECT COUNT(*) FROM reservations),':',(SELECT COUNT(*) FROM commit_receipts),':',(SELECT COUNT(*) FROM service_events),':',(SELECT SUM(steps_used) FROM harness_envelopes),':',(SELECT SUM(calls_used) FROM tasks))").Scan(&out); e != nil {
			t.Fatal(e)
		}
		return out
	}
	probe := func(boundary string, call func() error) {
		for _, mode := range []string{"ANCESTOR_EDIT", "ANCESTOR_LICENSE", "ANCESTOR_EXPIRY"} {
			t.Run(boundary+"/"+mode, func(t *testing.T) {
				var obj map[string]any
				if e := json.Unmarshal(original, &obj); e != nil {
					t.Fatal(e)
				}
				rights := obj["rights"].(map[string]any)
				switch mode {
				case "ANCESTOR_EDIT":
					rights["allowed_uses"] = []any{"COPY"}
				case "ANCESTOR_LICENSE":
					rights["license"] = " "
				case "ANCESTOR_EXPIRY":
					rights["expires_at"] = "2000-01-01T00:00:00Z"
				}
				raw, e := json.Marshal(obj)
				if e != nil {
					t.Fatal(e)
				}
				if _, e = r.Store.DB.Exec("UPDATE assets SET document=? WHERE asset_id=?", raw, assets[1]); e != nil {
					t.Fatal(e)
				}
				defer func() {
					if _, e := r.Store.DB.Exec("UPDATE assets SET document=? WHERE asset_id=?", original, assets[1]); e != nil {
						t.Error(e)
					}
				}()
				before, journal := counts(), journalBytes(t, r)
				e = call()
				if !errors.Is(e, content.ErrBlocked) && !errors.Is(e, store.ErrDenied) {
					t.Fatalf("expected current source qualification denial, got %T %v", e, e)
				}
				if counts() != before || !bytes.Equal(journal, journalBytes(t, r)) {
					t.Fatal("denial changed original lifecycle/provider")
				}
			})
		}
	}
	probe("EXTRA_AUTHORIZE", func() error { _, e := r.Store.AuthorizeDispatch(childCtx, d); return e })
	if _, e = r.Store.AuthorizeDispatch(childCtx, d); e != nil {
		t.Fatal("restored source extra authorization", e)
	}
	d.CommandKey = content.Identity(cl.TaskID, "repair-send")
	d.CommitKey = d.CommandKey
	probe("EXTRA_SEND", func() error { _, e := r.Store.StartSend(childCtx, d, d.AttemptID); return e })
	extra, e := r.Store.StartSend(childCtx, d, d.AttemptID)
	if e != nil {
		t.Fatal("restored source extra send", e)
	}
	r.Worker.Turns.Fault = 0
	if e = r.Worker.Turns.Generate(childCtx, extra.OriginalSend()); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.ObserveOriginalReply(childCtx, extra.OriginalSend()); e != nil {
		t.Fatal(e)
	}
	if e = r.Worker.Turns.ExecuteRehearsal(ctx, rl.TaskLease, worker); e != nil {
		t.Fatal("actual joint child/report after source restored", e)
	}
	body, e := r.Files.ReadCommittedRehearsalReport(ctx, scope, string(root.TaskID))
	if e != nil || len(body) == 0 {
		t.Fatal(e)
	}
	probe("TERMINAL_REPORT", func() error {
		b, e := r.Files.ReadCommittedRehearsalReport(ctx, scope, string(root.TaskID))
		if len(b) != 0 {
			t.Error("denial returned body")
		}
		return e
	})
	after, e := r.Files.ReadCommittedRehearsalReport(ctx, scope, string(root.TaskID))
	if e != nil || !bytes.Equal(body, after) {
		t.Fatal("original bytes after restored current use", e)
	}
	var calls, services int
	if e = r.Store.DB.QueryRow("SELECT calls_allocated,(SELECT COUNT(*) FROM service_events WHERE task_id=?) FROM harness_envelopes WHERE task_id=?", root.TaskID, root.TaskID).Scan(&calls, &services); e != nil || calls != 2 || services != 1 {
		t.Fatal(calls, services, e)
	}
}
