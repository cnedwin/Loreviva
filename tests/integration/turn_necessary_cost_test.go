package integration

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/app"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/harness"
	store "loreviva.local/loreviva/internal/store/mysql"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestNecessaryCostAfterDeadlineWithoutNarrativeAuthority(t *testing.T) {
	ctx := context.Background()
	r, scope, task, _ := originalResultFixture(t, true)
	id, e := r.Store.OriginalMockIdentity(ctx, scope, task)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.BeginNecessaryCostQuery(ctx, id); e == nil {
		t.Fatal("unnecessary cost query before exhaustion")
	}
	if _, e = r.Store.DB.Exec("UPDATE harness_envelopes SET accepted_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 121 SECOND),execute_by=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 SECOND),settle_by=DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 59 SECOND) WHERE task_id=?", task); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.DB.Exec("UPDATE memory_subject_permissions SET enabled=0 WHERE action='memory.generation.use'"); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.DB.Exec("UPDATE world_controls SET status='PAUSED',control_version=control_version+1 WHERE world_instance_id=?", scope.WorldInstanceID); e != nil {
		t.Fatal(e)
	}
	before := journalBytes(t, r)
	var execute, settle time.Time
	if e = r.Store.DB.QueryRow("SELECT execute_by,settle_by FROM harness_envelopes WHERE task_id=?", task).Scan(&execute, &settle); e != nil {
		t.Fatal(e)
	}
	q, e := r.Store.BeginNecessaryCostQuery(ctx, id)
	if e != nil {
		t.Fatal("necessary original cost blocked after deadline", e)
	}
	if _, e = r.Store.RecordOriginalQuery(ctx, q); e == nil {
		t.Fatal("cost lease escaped to full result metadata")
	}
	out, e := r.Store.RecordNecessaryCostQuery(ctx, q)
	if e != nil || out.Identity != id || out.State != "SUCCEEDED" || !out.UsageKnown || !out.NoFurtherCost || out.CostAtoms != 0 || out.CostUnit != "MONEY" || out.CostCurrency != "syntheticUSD" || out.InputUnits == 0 || out.OutputUnits == 0 {
		t.Fatal(out, e)
	}
	var ex, st time.Time
	var state string
	var calls, steps, services int
	if e = r.Store.DB.QueryRow("SELECT e.execute_by,e.settle_by,t.state,t.calls_used,e.steps_used,(SELECT COUNT(*) FROM service_events WHERE task_id=?) FROM harness_envelopes e JOIN tasks t USING(tenant_id,product_id,task_id) WHERE t.task_id=?", task, task).Scan(&ex, &st, &state, &calls, &steps, &services); e != nil {
		t.Fatal(e)
	}
	if ex != execute || st != settle || state != "RUNNING" || calls != 1 || steps != 3 || services != 0 || !bytes.Equal(before, journalBytes(t, r)) {
		t.Fatal("cost query mutated narrative", ex, st, state, calls, steps, services)
	}
	if _, _, e = r.Files.ReadApprovedTurn(ctx, scope, task); e == nil {
		t.Fatal("cost query approved output")
	}
	if _, e = r.Store.BeginNecessaryCostQuery(ctx, id); e == nil {
		t.Fatal("already complete cost queried again")
	}
	var records int
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_necessary_cost_records WHERE task_id=?", task).Scan(&records); e != nil || records != 1 {
		t.Fatal(records, e)
	}
}

func exhaustedOriginalQueryFixture(t *testing.T) (*app.Runtime, domain.MockRequestIdentity) {
	t.Helper()
	ctx := context.Background()
	r, scope, task, _ := actionResultFixture(t, harness.MockMalformedOriginalResult)
	lease, worker := currentResultLease(t, task, scope, r.Store.DB)
	d, e := r.Store.PrepareTurnRecovery(ctx, lease, worker)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.AuthorizeDispatch(ctx, d); e != nil {
		t.Fatal(e)
	}
	d.CommandKey = content.Identity(task, "cost-unsent")
	d.CommitKey = d.CommandKey
	if _, e = r.Store.StartSend(ctx, d, d.AttemptID); e != nil {
		t.Fatal(e)
	}
	id, e := r.Store.AttemptMockIdentity(ctx, scope, task, d.AttemptID)
	if e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 3; i++ {
		queryDue(t, r, task)
		q, e := r.Store.BeginOriginalQuery(ctx, id)
		if e != nil {
			t.Fatal(e)
		}
		m, e := r.Store.RecordOriginalQuery(ctx, q)
		if e != nil || m.State != "UNKNOWN" || m.UsageKnown {
			t.Fatal(m, e)
		}
	}
	return r, id
}
func costDue(t *testing.T, r *app.Runtime, id domain.MockRequestIdentity) {
	t.Helper()
	if _, e := r.Store.DB.Exec("UPDATE harness_original_queries SET next_cost_query_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 SECOND),lease_expires_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 SECOND) WHERE task_id=? AND attempt_id=?", id.TaskID, id.AttemptID); e != nil {
		t.Fatal(e)
	}
	if _, e := r.Store.DB.Exec("UPDATE harness_necessary_cost_records SET began_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 31 SECOND),next_query_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 SECOND) WHERE task_id=? AND attempt_id=?", id.TaskID, id.AttemptID); e != nil {
		t.Fatal(e)
	}
}
func TestNecessaryCostWorkerLifetimeBudgetSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	r, id := exhaustedOriginalQueryFixture(t)
	before := journalBytes(t, r)
	for i := 1; i <= 3; i++ {
		if e := r.Worker.QueryNecessaryCostOnce(ctx); e != nil {
			t.Fatal("necessary worker", i, e)
		}
		var foreground, background, offset int
		if e := r.Store.DB.QueryRow("SELECT q.queries_used,q.cost_queries_used,TIMESTAMPDIFF(SECOND,c.began_at,c.next_query_at) FROM harness_original_queries q JOIN harness_necessary_cost_records c USING(tenant_id,product_id,task_id,attempt_id) WHERE q.task_id=? AND q.attempt_id=? AND c.ordinal=?", id.TaskID, id.AttemptID, i).Scan(&foreground, &background, &offset); e != nil || foreground != 3 || background != i || offset != 30 {
			t.Fatal(foreground, background, offset, e)
		}
		if _, e := r.Store.BeginNecessaryCostQuery(ctx, id); e == nil {
			t.Fatal("30 second backoff ignored")
		}
		var raw []byte
		if e := r.Store.DB.QueryRow("SELECT metadata FROM harness_necessary_cost_records WHERE task_id=? AND attempt_id=? AND ordinal=?", id.TaskID, id.AttemptID, i).Scan(&raw); e != nil {
			t.Fatal(e)
		}
		var m map[string]json.RawMessage
		if json.Unmarshal(raw, &m) != nil {
			t.Fatal("bad cost record")
		}
		for _, key := range []string{"BodyStorage", "BodyHash", "BodyAvailable", "Canonical", "World"} {
			if _, ok := m[key]; ok {
				t.Fatal("narrative metadata leaked", key)
			}
		}
		var c store.NecessaryCostMetadata
		if json.Unmarshal(raw, &c) != nil || c.State != "UNKNOWN" || c.UsageKnown || c.NoFurtherCost {
			t.Fatal("UNKNOWN treated as zero authoritative cost", c)
		}
		if i == 1 {
			local, e := app.OpenLocal(ctx, r.Store, filepath.Dir(r.Local.Coordinator.Directory()), r.Files.RootPath)
			if e != nil {
				t.Fatal(e)
			}
			defer local.Participant.Close(ctx)
			recovered := app.NewRuntime(r.Store, r.Files, r.Auth)
			recovered.BindLocal(local)
			r = recovered
		}
		costDue(t, r, id)
	}
	if e := r.Worker.QueryNecessaryCostOnce(ctx); !errors.Is(e, store.ErrNoTask) {
		t.Fatal("background budget restarted", e)
	}
	if _, e := r.Store.BeginNecessaryCostQuery(ctx, id); e == nil {
		t.Fatal("fourth lifetime cost query")
	}
	var state string
	var calls, holds int
	if e := r.Store.DB.QueryRow("SELECT state,calls_used,(SELECT COUNT(*) FROM reservations WHERE task_id=? AND status='HELD') FROM tasks WHERE task_id=?", id.TaskID, id.TaskID).Scan(&state, &calls, &holds); e != nil || state != "RUNNING" || calls != 2 || holds != 8 {
		t.Fatal(state, calls, holds, e)
	}
	if !bytes.Equal(before, journalBytes(t, r)) {
		t.Fatal("cost worker generated provider request")
	}
}
func TestNecessaryCostCommitACKUnknownUsesExactReceipt(t *testing.T) {
	for _, phase := range []string{"begin", "record"} {
		t.Run(phase, func(t *testing.T) {
			ctx := context.Background()
			r, id := exhaustedOriginalQueryFixture(t)
			lost := func(ctx context.Context, tx *sql.Tx) error {
				if e := tx.Commit(); e != nil {
					return e
				}
				return errors.New("synthetic necessary cost ACK lost")
			}
			if phase == "begin" {
				r.Store.Commit = lost
			}
			lease, e := r.Store.BeginNecessaryCostQuery(ctx, id)
			r.Store.Commit = nil
			if phase == "begin" {
				if !errors.Is(e, store.ErrCommitUnknown) || lease.CostQueryKey() == "" {
					t.Fatal(e)
				}
				if _, e = r.Store.RecordNecessaryCostQuery(ctx, lease); e == nil {
					t.Fatal("unknown begin minted query permit")
				}
			} else {
				if e != nil {
					t.Fatal(e)
				}
				r.Store.Commit = lost
				_, e = r.Store.RecordNecessaryCostQuery(ctx, lease)
				r.Store.Commit = nil
				if !errors.Is(e, store.ErrCommitUnknown) {
					t.Fatal(e)
				}
			}
			receipt, e := r.Store.QueryNecessaryCostReceipt(ctx, id, lease.CostQueryKey())
			if e != nil || !receipt.BeginCommitted || receipt.Ordinal != 1 || receipt.RecordCommitted != (phase == "record") {
				t.Fatal(receipt, e)
			}
			if _, e = r.Store.QueryNecessaryCostReceipt(ctx, id, "substituted-query"); e == nil {
				t.Fatal("unrelated query receipt")
			}
			if _, e = r.Store.BeginNecessaryCostQuery(ctx, id); e == nil {
				t.Fatal("ACK loss reopened cost budget")
			}
			var n int
			if e = r.Store.DB.QueryRow("SELECT cost_queries_used FROM harness_original_queries WHERE task_id=? AND attempt_id=?", id.TaskID, id.AttemptID).Scan(&n); e != nil || n != 1 {
				t.Fatal(n, e)
			}
		})
	}
}

type boundedCostProvider struct {
	store.OriginalProviderEvidence
	cost    store.OriginalProviderCostEvidence
	entered chan struct{}
	once    sync.Once
	proceed chan struct{}
}

func (p *boundedCostProvider) OriginalCostMetadata(ctx context.Context, id domain.MockRequestIdentity) (domain.MockCostMetadata, error) {
	out, e := p.cost.OriginalCostMetadata(ctx, id)
	if e != nil {
		return out, e
	}
	p.once.Do(func() { close(p.entered) })
	select {
	case <-ctx.Done():
		return domain.MockCostMetadata{}, ctx.Err()
	case <-p.proceed:
		return out, nil
	}
}
func TestNecessaryCostCurrentServiceFenceAndFiveSecondBoundary(t *testing.T) {
	for _, mode := range []string{"old-fence", "service-before-IO", "service-after-IO", "generation", "five-seconds"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			r, id := exhaustedOriginalQueryFixture(t)
			first, e := r.Store.BeginNecessaryCostQuery(ctx, id)
			if e != nil {
				t.Fatal(e)
			}
			switch mode {
			case "old-fence":
				costDue(t, r, id)
				second, e := r.Store.BeginNecessaryCostQuery(ctx, id)
				if e != nil {
					t.Fatal(e)
				}
				if _, e = r.Store.RecordNecessaryCostQuery(ctx, first); e == nil {
					t.Fatal("old fence used")
				}
				if _, e = r.Store.RecordNecessaryCostQuery(ctx, second); e != nil {
					t.Fatal(e)
				}
				return
			case "service-before-IO":
				if _, e = r.Store.DB.Exec("UPDATE harness_query_services SET enabled=0"); e != nil {
					t.Fatal(e)
				}
			case "generation":
				if _, e = r.Store.DB.Exec("UPDATE memory_installation SET generation=generation+1"); e != nil {
					t.Fatal(e)
				}
			case "service-after-IO", "five-seconds":
				provider := &boundedCostProvider{OriginalProviderEvidence: r.Store.ProviderEvidence, cost: r.Store.ProviderEvidence.(store.OriginalProviderCostEvidence), entered: make(chan struct{}), proceed: make(chan struct{})}
				r.Store.ProviderEvidence = provider
				done := make(chan error, 1)
				started := time.Now()
				go func() { _, err := r.Store.RecordNecessaryCostQuery(ctx, first); done <- err }()
				select {
				case <-provider.entered:
				case <-time.After(2 * time.Second):
					t.Fatal("provider never entered")
				}
				if mode == "service-after-IO" {
					if _, e = r.Store.DB.Exec("UPDATE harness_query_services SET enabled=0"); e != nil {
						t.Fatal(e)
					}
					close(provider.proceed)
				}
				select {
				case e = <-done:
				case <-time.After(7 * time.Second):
					t.Fatal("bounded query hung")
				}
				if e == nil {
					t.Fatal("IO boundary ignored", mode)
				}
				if mode == "five-seconds" {
					if !errors.Is(e, context.DeadlineExceeded) || time.Since(started) < 4500*time.Millisecond || time.Since(started) > 6*time.Second {
						t.Fatal("not actual five second query ceiling", time.Since(started), e)
					}
				}
				var recorded int
				if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_necessary_cost_records WHERE task_id=? AND attempt_id=? AND metadata IS NOT NULL", id.TaskID, id.AttemptID).Scan(&recorded); e != nil || recorded != 0 {
					t.Fatal(recorded, e)
				}
				return
			}
			if _, e = r.Store.RecordNecessaryCostQuery(ctx, first); e == nil {
				t.Fatal("current authority ignored", mode)
			}
			if _, e = r.Store.BeginNecessaryCostQuery(ctx, id); e == nil {
				t.Fatal("invalid authority got new query", mode)
			}
		})
	}
}

func TestNecessaryCostLeaseCannotRepeatProviderIO(t *testing.T) {
	ctx := context.Background()
	r, id := exhaustedOriginalQueryFixture(t)
	lease, e := r.Store.BeginNecessaryCostQuery(ctx, id)
	if e != nil {
		t.Fatal(e)
	}
	provider := &boundedCostProvider{OriginalProviderEvidence: r.Store.ProviderEvidence, cost: r.Store.ProviderEvidence.(store.OriginalProviderCostEvidence), entered: make(chan struct{}), proceed: make(chan struct{})}
	r.Store.ProviderEvidence = provider
	first := make(chan error, 1)
	go func() { _, err := r.Store.RecordNecessaryCostQuery(ctx, lease); first <- err }()
	<-provider.entered
	second := make(chan error, 1)
	go func() { _, err := r.Store.RecordNecessaryCostQuery(ctx, lease); second <- err }()
	repeated := false
	select {
	case e = <-second:
		if e == nil {
			repeated = true
		}
	case <-time.After(100 * time.Millisecond):
		repeated = true
	}
	close(provider.proceed)
	if e = <-first; e != nil {
		t.Fatal(e)
	}
	if repeated {
		<-second
		t.Fatal("same opaque cost lease entered provider IO twice")
	}
}

type boundedOriginalProvider struct {
	store.OriginalProviderEvidence
	entered, proceed chan struct{}
	once             sync.Once
}

func (p *boundedOriginalProvider) OriginalMetadata(ctx context.Context, id domain.MockRequestIdentity) (domain.MockResultMetadata, error) {
	out, e := p.OriginalProviderEvidence.OriginalMetadata(ctx, id)
	if e != nil {
		return out, e
	}
	p.once.Do(func() { close(p.entered) })
	select {
	case <-ctx.Done():
		return domain.MockResultMetadata{}, ctx.Err()
	case <-p.proceed:
		return out, nil
	}
}
func TestOriginalQueryLeaseCannotRepeatProviderIO(t *testing.T) {
	ctx := context.Background()
	r, scope, task, _ := originalResultFixture(t, true)
	id, e := r.Store.OriginalMockIdentity(ctx, scope, task)
	if e != nil {
		t.Fatal(e)
	}
	queryDue(t, r, task)
	lease, e := r.Store.BeginOriginalQuery(ctx, id)
	if e != nil {
		t.Fatal(e)
	}
	provider := &boundedOriginalProvider{OriginalProviderEvidence: r.Store.ProviderEvidence, entered: make(chan struct{}), proceed: make(chan struct{})}
	r.Store.ProviderEvidence = provider
	first := make(chan error, 1)
	go func() { _, err := r.Store.RecordOriginalQuery(ctx, lease); first <- err }()
	<-provider.entered
	second := make(chan error, 1)
	go func() { _, err := r.Store.RecordOriginalQuery(ctx, lease); second <- err }()
	repeated := false
	select {
	case e = <-second:
		if e == nil {
			repeated = true
		}
	case <-time.After(100 * time.Millisecond):
		repeated = true
	}
	close(provider.proceed)
	firstErr := <-first
	if repeated {
		<-second
		t.Fatal("same original query lease entered provider IO twice")
	}
	if firstErr != nil {
		t.Fatal(firstErr)
	}
}
