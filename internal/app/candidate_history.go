package app

import (
	"context"
	"errors"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/maintenance"
	"net/http"
)

// Used only by named independently authorized read operations. It retains
// all installation checks; general readiness and execution auth remain strict.
func (r *Runtime) committedArtifactReadReady(ctx context.Context) error {
	if r == nil || r.Store == nil || r.Files == nil || r.Auth == nil || r.Worker == nil || r.Local == nil || r.Local.Mock == nil || r.Local.Ledger == nil {
		return errors.New("committed artifact history unavailable")
	}
	if e := r.Local.Participant.Check(ctx); e != nil {
		return e
	}
	if e := r.Local.Mock.Ready(ctx); e != nil {
		return e
	}
	if e := r.Memory.CheckResultGovernance(ctx); e != nil {
		return e
	}
	if e := maintenance.Compatible(ctx, r.Store); e != nil {
		return e
	}
	if e := maintenance.GovernanceReady(ctx, r.Store); e != nil {
		return e
	}
	if e := r.Files.Check(ctx); e != nil {
		return e
	}
	return r.Worker.Check(ctx)
}

func candidateHistoryQuery(q *http.Request) bool {
	if q.Method != "GET" {
		return false
	}
	for _, v := range q.URL.Query() {
		if len(v) != 1 {
			return false
		}
	}
	return true
}

func (r *Runtime) candidateTaskHistory(w http.ResponseWriter, q *http.Request) bool {
	if !candidateHistoryQuery(q) {
		return false
	}
	s, e := r.Auth.AuthorizeCommittedCandidateTask(q.Context(), q.Header.Get("Authorization"), q.URL.Query().Get("product_id"), q.PathValue("task_id"))
	if e != nil {
		return false
	}
	if e = r.committedArtifactReadReady(q.Context()); e != nil {
		failure(w, e)
		return true
	}
	v, e := r.Store.ReadDraftCandidateTask(q.Context(), s, q.PathValue("task_id"))
	if e != nil {
		failure(w, e)
		return true
	}
	if _, e = r.Auth.AuthorizeCommittedCandidateTask(q.Context(), q.Header.Get("Authorization"), q.URL.Query().Get("product_id"), q.PathValue("task_id")); e != nil {
		failure(w, e)
		return true
	}
	response(w, 200, v)
	return true
}

func (r *Runtime) assetReaderScope(ctx context.Context, q *http.Request) (domain.ProductScope, error) {
	if candidateHistoryQuery(q) {
		s, e := r.Auth.AuthorizeCommittedCandidateAsset(ctx, q.Header.Get("Authorization"), q.URL.Query().Get("product_id"), q.PathValue("asset_id"))
		if e == nil {
			return s, r.committedArtifactReadReady(ctx)
		}
		s, e = r.Auth.AuthorizeCommittedRehearsalReportAsset(ctx, q.Header.Get("Authorization"), q.URL.Query().Get("product_id"), q.PathValue("asset_id"))
		if e == nil {
			return s, r.committedArtifactReadReady(ctx)
		}
	}
	s, e := r.Auth.Authorize(ctx, q.Header.Get("Authorization"), q.URL.Query().Get("product_id"), "asset.read")
	if e != nil {
		return s, e
	}
	return s, r.Readiness(ctx)
}
