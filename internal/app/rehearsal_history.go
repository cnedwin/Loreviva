package app

import "net/http"

func (r *Runtime) rehearsalTaskHistory(w http.ResponseWriter, q *http.Request) bool {
	if !candidateHistoryQuery(q) {
		return false
	}
	s, e := r.Auth.AuthorizeCommittedRehearsalTask(q.Context(), q.Header.Get("Authorization"), q.URL.Query().Get("product_id"), q.PathValue("task_id"))
	if e != nil {
		return false
	}
	if e = r.committedArtifactReadReady(q.Context()); e != nil {
		failure(w, e)
		return true
	}
	v, e := r.Store.ReadDraftRehearsalTask(q.Context(), s, q.PathValue("task_id"))
	if e != nil {
		failure(w, e)
		return true
	}
	if _, e = r.Auth.AuthorizeCommittedRehearsalTask(q.Context(), q.Header.Get("Authorization"), q.URL.Query().Get("product_id"), q.PathValue("task_id")); e != nil {
		failure(w, e)
		return true
	}
	response(w, 200, v)
	return true
}
