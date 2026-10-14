package harness

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/maintenance"
	store "loreviva.local/loreviva/internal/store/mysql"
)

func (x *Executor) RehearsalReady() bool {
	return x.CandidateReady() && x.Turns.Store.DraftRehearsalOwner != nil && x.Turns.Store.DraftRehearsalReader != nil && x.Turns.Store.RehearsalInitializer != nil && x.Turns.Store.RehearsalInputOwner != nil && x.Turns.Store.RehearsalReportReader != nil
}

// The only registered root handler. Children are claimed through the owning
// root lease; ordinary queue claims cannot enroll an isolated child.
func (x *Executor) ExecuteRehearsal(ctx context.Context, root domain.TaskLease, worker string) (err error) {
	phase := "admission"
	defer func() {
		if err != nil {
			err = fmt.Errorf("REHEARSAL %s: %w", phase, err)
		}
	}()
	if !x.RehearsalReady() || root.WorldInstanceID != nil {
		return store.ErrDenied
	}
	ctx, close, e := x.Participant.Enter(ctx, maintenance.HarnessWriter)
	if e != nil {
		return e
	}
	defer close()
	s := x.Turns.Store
	ctx, e = s.EnterDraftTaskIssuer(ctx, root, worker)
	if e != nil {
		return e
	}
	if _, e = s.QueryRehearsalReportCommit(ctx, root.ProductScope, root.TaskID); e == nil {
		return nil
	} else if !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	phase = "initialize"
	if _, e = s.InitializeDraftRehearsal(ctx, root, worker); e != nil {
		return e
	}
	for executed := 0; executed <= 20; executed++ {
		phase = "original ordered input"
		var raw []byte
		var next int
		if e = s.DB.QueryRowContext(ctx, "SELECT binding,next_input FROM harness_admin_rehearsals WHERE tenant_id=? AND product_id=? AND task_id=? AND owner_subject_id=?", root.TenantID, root.ProductID, root.TaskID, root.SubjectID).Scan(&raw, &next); e != nil {
			return e
		}
		var binding domain.RehearsalBinding
		if json.Unmarshal(raw, &binding) != nil || len(binding.Inputs) < 1 || len(binding.Inputs) > 20 || next < 0 || next > len(binding.Inputs) {
			return store.ErrDenied
		}
		if next == len(binding.Inputs) {
			phase = "joint report commit"
			_, e = s.CommitRehearsalReport(ctx, root, worker)
			return e
		}
		if executed == 20 {
			return store.ErrDenied
		}
		// Each ordered input uses the owning root's claim, bound child context and execution entry; require exactly one input advance before continuing or committing the report.
		phase = "fixed child claim"
		child, e := s.ClaimRehearsalChild(ctx, root, worker)
		if e != nil {
			return e
		}
		phase = "child original context"
		if _, e = s.BindRehearsalChildContext(ctx, root, child.TaskLease, worker); e != nil {
			return e
		}
		phase = "child original execution"
		childCtx, release, e := s.EnterRehearsalChildExecution(ctx, root, child.TaskLease, worker)
		if e != nil {
			return e
		}
		e = x.Execute(childCtx, child.TaskLease, worker)
		release()
		if e != nil {
			return e
		}
		var advanced int
		if e = s.DB.QueryRowContext(ctx, "SELECT next_input FROM harness_admin_rehearsals WHERE tenant_id=? AND product_id=? AND task_id=?", root.TenantID, root.ProductID, root.TaskID).Scan(&advanced); e != nil {
			return e
		}
		if advanced != next+1 {
			return store.ErrDenied
		}
	}
	return store.ErrDenied
}
