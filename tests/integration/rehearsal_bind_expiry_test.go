package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/harness"
	store "loreviva.local/loreviva/internal/store/mysql"
)

// The observer delegates every evidence read to the real Files owner. It is
// installed only after the extra send marker, to isolate the observed bind seam.
type expiryObservedReader struct {
	store.OriginalResultReader
	before func(context.Context, domain.TaskLease, string)
}

func (r *expiryObservedReader) ReadOriginalEvidence(ctx context.Context, lease domain.TaskLease, worker string) (store.OriginalResultEvidence, error) {
	if r.before != nil {
		r.before(ctx, lease, worker)
	}
	return r.OriginalResultReader.ReadOriginalEvidence(ctx, lease, worker)
}

func TestRehearsalLeaseExpiryAtExtraBodyBindingPreservesOriginalQuery(t *testing.T) {
	ctx, r, root, child, d, _, closeProof := skillPrefixOwned(t, "")
	defer closeProof()
	worker := "skill-worker"
	if _, e := r.Store.AuthorizeDispatch(ctx, d); e != nil {
		t.Fatal(e)
	}
	d.CommandKey, d.CommitKey = "bind-seam-original", "bind-seam-original"
	auth, e := r.Store.StartSend(ctx, d, d.AttemptID)
	if e != nil {
		t.Fatal(e)
	}
	r.Worker.Turns.Fault = harness.MockMalformedOriginalResult
	if e = r.Worker.Turns.Generate(ctx, auth.OriginalSend()); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.ObserveOriginalReply(ctx, auth.OriginalSend()); e != nil {
		t.Fatal(e)
	}
	if e = r.Store.ValidateOriginalTurn(ctx, child, worker); e != nil {
		t.Fatal(e)
	}
	extra, e := r.Store.PrepareTurnRecovery(ctx, child, worker)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.AuthorizeDispatch(ctx, extra); e != nil {
		t.Fatal(e)
	}
	extra.CommandKey, extra.CommitKey = "bind-seam-extra", "bind-seam-extra"
	extraAuth, e := r.Store.StartSend(ctx, extra, extra.AttemptID)
	if e != nil {
		t.Fatal(e)
	}
	reader := r.Store.OriginalResultReader
	defer func() { r.Store.OriginalResultReader = reader }()
	observed := false
	r.Store.OriginalResultReader = &expiryObservedReader{OriginalResultReader: reader, before: func(c context.Context, lease domain.TaskLease, w string) {
		if observed {
			return
		}
		// Permit.Take also reads original evidence. Wait for the real provider
		// journal begin, so this observation cannot interrupt request admission.
		began := false
		for _, line := range bytes.Split(journalBytes(t, r), []byte{'\n'}) {
			var entry struct{ Result domain.MockResultMetadata }
			if len(line) == 0 {
				continue
			}
			if err := json.Unmarshal(line, &entry); err != nil {
				t.Fatal(err)
			}
			began = began || entry.Result.Identity.AttemptID == extra.AttemptID && entry.Result.State == "UNKNOWN"
		}
		if !began {
			return
		}
		if lease.TaskID != child.TaskID || w != worker {
			t.Fatal("wrong bind seam")
		}
		valid, err := reader.ReadOriginalEvidence(c, lease, w)
		if err != nil {
			t.Fatal("real pre-expiry evidence", err)
		}
		valid.Close()
		// Expire only this fixture's exact leases. Do not change fences,
		// execute_by, source permissions, result bodies or provider metadata.
		if _, err = r.Store.DB.ExecContext(c, "UPDATE tasks SET lease_expires_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 MICROSECOND) WHERE task_id IN (?,?)", root.TaskID, child.TaskID); err != nil {
			t.Fatal(err)
		}
		_, inner := r.Memory.AuthorizeAttemptResult(c, lease, w, d.AttemptID)
		t.Logf("bind seam owning AuthorizeAttemptResult=%v denied=%v ctx=%v", inner, errors.Is(inner, store.ErrDenied), c.Err())
		if !errors.Is(inner, store.ErrDenied) || c.Err() != nil {
			t.Fatal("not isolated lease denial", inner, c.Err())
		}
		observed = true
	}}
	e = r.Worker.Turns.Generate(ctx, extraAuth.OriginalSend())
	t.Logf("actual extra Generate after lease expiry: %v", e)
	if !observed || e == nil {
		t.Fatal("missing real bind rejection", observed, e)
	}
	r.Store.OriginalResultReader = reader
	var completion domain.MockResultMetadata
	for _, line := range bytes.Split(journalBytes(t, r), []byte{'\n'}) {
		var entry struct{ Result domain.MockResultMetadata }
		if len(line) == 0 {
			continue
		}
		if e = json.Unmarshal(line, &entry); e != nil {
			t.Fatal(e)
		}
		if entry.Result.Identity.AttemptID == extra.AttemptID && entry.Result.State == "SUCCEEDED" {
			completion = entry.Result
		}
	}
	if completion.State != "SUCCEEDED" || completion.BodyAvailable || completion.BodyStorage != "" || !completion.UsageKnown {
		t.Fatal("original completion fact", completion.State, completion.BodyAvailable, completion.BodyStorage, completion.UsageKnown)
	}
	queryDue(t, r, child.TaskID)
	q, e := r.Store.BeginOriginalQuery(context.Background(), completion.Identity)
	if e != nil {
		t.Fatal("original query after lease expiry", e)
	}
	if _, e = r.Store.RecordOriginalQuery(context.Background(), q); e != nil {
		t.Fatal("original accounting after lease expiry", e)
	}
	before := journalBytes(t, r)
	closeProof()
	newRoot, e := r.Store.ClaimRegistered(context.Background(), "bind-takeover", time.Now(), time.Minute, []store.RegisteredOperation{{Kind: "ADMIN", Operation: "REHEARSAL", TaskID: root.TaskID}})
	if e != nil {
		t.Fatal(e)
	}
	issuer, e := r.Store.EnterDraftTaskIssuer(context.Background(), newRoot.TaskLease, "bind-takeover")
	if e != nil {
		t.Fatal(e)
	}
	newChild, e := r.Store.ClaimRehearsalChild(issuer, newRoot.TaskLease, "bind-takeover")
	if e != nil {
		t.Fatal(e)
	}
	fresh, release, e := r.Store.EnterRehearsalChildExecution(issuer, newRoot.TaskLease, newChild.TaskLease, "bind-takeover")
	if e != nil {
		t.Fatal(e)
	}
	defer release()
	e = r.Worker.Turns.Execute(fresh, newChild.TaskLease, "bind-takeover")
	t.Logf("actual takeover result after original SUCCEEDED without retained body: %v", e)
	if e == nil {
		t.Fatal("unretained result became an approved narrative")
	}
	var calls, bindings int
	if err := r.Store.DB.QueryRow("SELECT calls_used,(SELECT COUNT(*) FROM harness_result_sources WHERE task_id=? AND attempt_id=?) FROM tasks WHERE task_id=?", child.TaskID, extra.AttemptID, child.TaskID).Scan(&calls, &bindings); err != nil || calls != 2 || bindings != 0 {
		t.Fatal(calls, bindings, err)
	}
	if !bytes.Equal(before, journalBytes(t, r)) {
		t.Fatal("takeover generated again")
	}
	t.Log("observed exact original query/accounting survives; no fabricated body, no new purchase; current takeover cannot validate the unretained extra")
}
