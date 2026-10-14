package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"loreviva.local/loreviva/internal/app"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/harness"
)

// The actual HTTP operation must retain admission until its last response byte.
// This writer blocks transport only, after the owning read has produced a result.
type heldParticipantResponse struct {
	*httptest.ResponseRecorder
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *heldParticipantResponse) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered); <-w.release })
	return w.ResponseRecorder.Write(p)
}

func currentParticipantFixture(t *testing.T) (*app.Runtime, string, string, domain.MockRequestIdentity, []string) {
	t.Helper()
	r, draft, sources, req := physicalRehearsalDraft(t)
	ctx := runtimeContext(t, r)
	scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
	req.ExpectedDraftRevision = attachOnlineTextSkill(t, r, draft, "GenerationMaterial")
	root, e := r.Store.AcceptDraftRehearsal(ctx, scope, draft, "current-c-repair", req)
	if e != nil {
		t.Fatal(e)
	}
	r.Worker.Turns.Fault = harness.MockMalformedOriginalResult
	if e = r.Worker.Once(ctx); e != nil {
		t.Fatal(e)
	}
	negative, e := r.Store.AcceptDraftRehearsal(ctx, scope, draft, "current-c-negative", req)
	if e != nil {
		t.Fatal(e)
	}
	r.Worker.Turns.Fault = harness.MockRefuseOriginalResult
	if e = r.Worker.Once(ctx); e != nil {
		t.Fatal(e)
	}
	d, e := r.Content.GetDraft(ctx, scope, draft, "")
	if e != nil {
		t.Fatal(e)
	}
	candidate := generated.CandidateRequest{ProductID: "coffee", ExpectedDraftRevision: d.DraftRevision, Mode: "MOCK", ModuleID: "AUTHORING", ObjectID: d.Input.RootObjectID, SelectedFields: []string{"description"}, ApprovedCostUnits: "0"}
	pending := addPendingCandidateMeter(t, r, draft, candidate, "current-c-pending", false)
	var inv, neg, meter, lineage int
	if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM harness_skill_invocations),(SELECT COUNT(*) FROM harness_rehearsal_negative_outcomes WHERE root_task_id=?),(SELECT COUNT(*) FROM harness_attempt_metering WHERE usage_state='UNKNOWN'),(SELECT COUNT(*) FROM memory_asset_lineage)", negative.TaskID).Scan(&inv, &neg, &meter, &lineage); e != nil || inv < 3 || neg != 1 || meter != 1 || lineage < 3 {
		t.Fatal("actual current domains", inv, neg, meter, lineage, e)
	}
	var asset string
	if e = r.Store.DB.QueryRow("SELECT asset_id FROM harness_rehearsal_reports WHERE task_id=?", root.TaskID).Scan(&asset); e != nil {
		t.Fatal(e)
	}
	return r, draft, asset, pending, sources
}

func TestCurrentParticipantsCombinedReadersAndStreamDrain(t *testing.T) {
	r, draft, asset, pending, _ := currentParticipantFixture(t)
	ctx := runtimeContext(t, r)
	scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
	d, e := r.Content.GetDraft(ctx, scope, draft, "")
	if e != nil {
		t.Fatal(e)
	}
	stream, e := r.Files.OpenAuthorized(ctx, scope, asset)
	if e != nil {
		t.Fatal(e)
	}
	defer stream.Close()
	one := make([]byte, 1)
	if n, e := stream.Read(one); e != nil || n != 1 {
		t.Fatal("valid original stream", n, e)
	}
	counts := func() [4]int {
		t.Helper()
		var v [4]int
		if e := r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM commit_receipts),(SELECT COUNT(*) FROM service_events),SUM(calls_allocated),SUM(steps_used) FROM harness_envelopes").Scan(&v[0], &v[1], &v[2], &v[3]); e != nil {
			t.Fatal(e)
		}
		return v
	}
	before := counts()
	resolver, _ := json.Marshal(generated.DraftGenerationResolutionRequest{ProductID: "coffee", ExpectedDraftRevision: d.DraftRevision, ObjectID: d.Input.RootObjectID, ModuleID: "AUTHORING"})
	requests := []struct {
		method, path string
		body         []byte
	}{
		{"POST", "/world/v1/admin/content-drafts/" + draft + "/generation-resolution", resolver},
		{"GET", "/world/v1/admin/tasks?product_id=coffee", nil},
		{"GET", "/world/v1/admin/tasks/" + pending.TaskID + "?product_id=coffee", nil},
	}
	writers := []*heldParticipantResponse{}
	finished := make(chan int, 3)
	for i, spec := range requests {
		w := &heldParticipantResponse{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: make(chan struct{})}
		writers = append(writers, w)
		defer func() {
			select {
			case <-w.release:
			default:
				close(w.release)
			}
		}()
		q := httptest.NewRequest(spec.method, spec.path, bytes.NewReader(spec.body))
		q.Header.Set("Authorization", "Bearer "+runtimeToken)
		q.Header.Set("Content-Type", "application/json")
		go func(i int, w *heldParticipantResponse, q *http.Request) { r.Handler().ServeHTTP(w, q); finished <- i }(i, w, q)
		select {
		case <-w.entered:
		case <-time.After(5 * time.Second):
			t.Fatal("current reader did not produce response", i)
		}
		if w.Code != 200 {
			close(w.release)
			<-finished
			t.Fatal("positive owning read", i, w.Code, w.Body.String())
		}
	}
	closing, stop := context.WithTimeout(ctx, 8*time.Second)
	defer stop()
	if e = r.Local.Coordinator.BeginClose(closing, "combined-current-c"); e != nil {
		t.Fatal(e)
	}
	drained := make(chan error, 1)
	go func() { drained <- r.Local.Coordinator.Drain(closing, "combined-current-c") }()
	select {
	case e := <-drained:
		t.Fatal("drain crossed actual reader/stream", e)
	case <-time.After(80 * time.Millisecond):
	}
	for _, spec := range requests {
		q := httptest.NewRequest(spec.method, spec.path, bytes.NewReader(spec.body))
		q.Header.Set("Authorization", "Bearer "+runtimeToken)
		q.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.Handler().ServeHTTP(w, q)
		if w.Code != 503 {
			t.Fatal("closing admitted new read", w.Code, w.Body.String())
		}
	}
	for _, w := range writers {
		close(w.release)
	}
	for range writers {
		select {
		case <-finished:
		case <-closing.Done():
			t.Fatal(closing.Err())
		}
	}
	select {
	case e := <-drained:
		t.Fatal("drain ignored still-open actual Files stream", e)
	case <-time.After(80 * time.Millisecond):
	}
	if n, e := stream.Read(one); e == nil || n != 0 {
		t.Fatal("retired stream delivered bytes", n, e)
	}
	if e = stream.Close(); e != nil {
		t.Fatal(e)
	}
	select {
	case e = <-drained:
		if e != nil {
			t.Fatal(e)
		}
	case <-closing.Done():
		t.Fatal("current admission leaked", closing.Err())
	}
	if after := counts(); after != before {
		t.Fatal("pure reads changed facts", before, after)
	}
}
