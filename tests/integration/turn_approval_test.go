package integration

import (
	"context"
	"testing"
)

func TestActualOriginalCandidateRequiresProgramApproval(t *testing.T) {
	r, scope, task, _ := originalResultFixture(t, false)
	lease, worker := currentResultLease(t, task, scope, r.Store.DB)
	if e := r.Store.ApproveOriginalTurn(context.Background(), lease, worker); e != nil {
		t.Fatal("genuine candidate did not receive program approval", e)
	}
	var state string
	if e := r.Store.DB.QueryRow("SELECT fsm_state FROM harness_turns WHERE task_id=?", task).Scan(&state); e != nil || state != "APPROVED" {
		t.Fatal(state, e)
	}
	var count int
	if e := r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_candidate_approvals WHERE task_id=?", task).Scan(&count); e != nil || count != 1 {
		t.Fatal(count, e)
	}
}
