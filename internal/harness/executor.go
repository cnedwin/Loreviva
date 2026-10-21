package harness

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/files"
	"loreviva.local/loreviva/internal/maintenance"
	store "loreviva.local/loreviva/internal/store/mysql"
	"time"
	"unicode/utf8"
)

type Executor struct {
	Fault       MockFault
	Turns       *TurnService
	Files       *files.Store
	Journal     *MockJournal
	Participant *maintenance.Participant
}

type MockFault uint8

const (
	MockDropOriginalReply         MockFault = 1
	MockDelayResultStage          MockFault = 2
	MockMalformedOriginalResult   MockFault = 3
	MockRefuseOriginalResult      MockFault = 4
	MockMalformedEveryResult      MockFault = 21
	MockAcceptedOutcomeUnknown    MockFault = 22
	MockUsagePendingRefusal       MockFault = 23
	MockUngovernedActionNote      MockFault = 5
	MockWrongRelationshipMemory   MockFault = 6
	MockLateTwoBillItems          MockFault = 7
	MockLateConflictingBillItem   MockFault = 8
	MockDraftDuplicateField       MockFault = 9
	MockDraftTrailingJSON         MockFault = 10
	MockDraftUnselectedField      MockFault = 11
	MockDraftInvalidFieldType     MockFault = 12
	MockDraftOverlongField        MockFault = 13
	MockDraftInvalidUTF8          MockFault = 14
	MockCostFinalityPending       MockFault = 15
	MockLateFinalZeroBill         MockFault = 16
	MockUsagePending              MockFault = 17
	MockUsagePendingRepair        MockFault = 18
	MockCostFinalityPendingRepair MockFault = 20
)

var ErrMockReplyLost = errors.New("original Mock reply lost; query original request")

func (x *Executor) QueryOnce(ctx context.Context) error {
	if x == nil || x.Participant == nil {
		return store.ErrDenied
	}
	ctx, release, e := x.Participant.Enter(ctx, maintenance.QueryWriter)
	if e != nil {
		return e
	}
	defer release()
	id, e := x.Turns.Store.DueOriginalQuery(ctx)
	if e != nil {
		return e
	}
	lease, e := x.Turns.Store.BeginOriginalQuery(ctx, id)
	if e != nil {
		return e
	}
	_, e = x.Turns.Store.RecordOriginalQuery(ctx, lease)
	return e
}

func (x *Executor) Execute(ctx context.Context, lease domain.TaskLease, worker string) (err error) {
	phase := "admission"
	defer func() {
		if err != nil {
			err = fmt.Errorf("TURN %s: %w", phase, err)
		}
	}()
	if x == nil || x.Turns == nil || x.Files == nil || x.Journal == nil || x.Participant == nil || lease.WorldInstanceID == nil {
		return store.ErrDenied
	}
	ctx, release, e := x.Participant.Enter(ctx, maintenance.HarnessWriter)
	if e != nil {
		return e
	}
	defer release()
	ctx, e = x.Turns.Store.EnterIntegrationTaskIssuer(ctx, lease, worker)
	if e != nil {
		return e
	}
	scope := domain.Scope{ProductScope: lease.ProductScope, WorldInstanceID: *lease.WorldInstanceID}
	if d, e := x.Turns.Store.RecoveryTurnDispatch(ctx, lease, worker); e == nil {
		return x.executeRecoveryDispatch(ctx, lease, worker, d)
	} else if !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	id, e := x.Turns.Store.OriginalMockIdentity(ctx, scope, lease.TaskID)
	if e == nil {
		if _, e := x.Turns.Store.QueryOriginalValidation(ctx, scope, lease.TaskID); e == nil {
			return x.completeOriginal(ctx, lease, worker)
		} else if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		meta, e := x.originalResult(ctx, id)
		if e != nil || meta.State != "SUCCEEDED" {
			return e
		}
		if e = x.Turns.Store.ValidateOriginalTurn(ctx, lease, worker); e != nil {
			return e
		}
		return x.completeOriginal(ctx, lease, worker)
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	phase = "context"
	d, e := x.Turns.PrepareOriginalTurn(ctx, lease, worker)
	if e != nil {
		return e
	}
	phase = "authorize"
	if _, e = x.Turns.Store.AuthorizeDispatch(ctx, d); e != nil {
		return e
	}
	d.CommandKey = content.Identity(lease.TaskID, "send")
	d.CommitKey = d.CommandKey
	phase = "marker"
	auth, e := x.Turns.Store.StartSend(ctx, d, d.AttemptID)
	if e != nil {
		return e
	}
	if !auth.CanSend || auth.OriginalSend() == nil {
		return store.ErrDenied
	}
	phase = "generate"
	if e = x.Generate(ctx, auth.OriginalSend()); e != nil {
		return e
	}
	phase = "reply"
	if x.Fault == MockDropOriginalReply {
		return ErrMockReplyLost
	}
	_, e = x.Turns.Store.ObserveOriginalReply(ctx, auth.OriginalSend())
	if e != nil {
		return e
	}
	phase = "validation"
	if e = x.Turns.Store.ValidateOriginalTurn(ctx, lease, worker); e != nil {
		return e
	}
	return x.completeOriginal(ctx, lease, worker)
}

// Generate can only consume the fresh, in-process known-marker capability.
// Every retry/restart enters the original metadata query path instead.
func (x *Executor) Generate(ctx context.Context, permit *store.OriginalSendPermit) (err error) {
	if x == nil || x.Participant == nil || x.Turns == nil || x.Files == nil || x.Journal == nil {
		return store.ErrDenied
	}
	phase := "request"
	defer func() {
		if err != nil {
			err = fmt.Errorf("Mock %s: %w", phase, err)
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ctx, release, e := x.Participant.Enter(ctx, maintenance.MockWriter)
	if e != nil {
		return e
	}
	defer release()
	d, m, e := permit.Take(ctx, x.Turns.Store)
	if e != nil {
		return e
	}
	h := sha256.Sum256(m.Canonical)
	input := uint64(utf8.RuneCount(m.Canonical))
	module := "INTERACTION"
	if d.IsRehearsalChild() {
		module = "REHEARSAL"
	}
	if m.Connection != content.MockConnection || m.Model != content.MockModel || m.Module != module || hex.EncodeToString(h[:]) != m.Binding.RequestHash || input == 0 || input > m.InputBound || m.InputBound > store.MockInputLimit || m.OutputBound == 0 || m.OutputBound > store.MockOutputLimit {
		return store.ErrDenied
	}
	id := domain.MockRequestIdentity{AuthorizationID: d.AuthorizationID, Scope: *d.WorldScope, TaskID: d.TaskID, AttemptID: d.AttemptID, RequestKey: d.RequestKey, RequestHash: m.Binding.RequestHash, RouteID: d.Route, QuoteID: d.HarnessQuoteID, MarkerKey: d.CommitKey}
	phase = "journal begin"
	if e = x.Journal.begin(ctx, id); e != nil {
		return e
	}
	if x.Fault == MockAcceptedOutcomeUnknown {
		return ErrMockReplyLost
	}
	raw, e := mockCandidate(m.Canonical)
	if e != nil {
		return e
	}
	// Closed synthetic untrusted-output faults still travel through real J,
	// governed storage and the owning semantic validator.
	if x.Fault == MockUngovernedActionNote || x.Fault == MockWrongRelationshipMemory {
		var c Candidate
		if json.Unmarshal(raw, &c) != nil || len(c.Actions) != 1 || len(c.MemoryProposals) != 1 {
			return store.ErrDenied
		}
		if x.Fault == MockUngovernedActionNote {
			c.Actions[0].Text = "synthetic private text must not be copied to an event"
		} else {
			c.MemoryProposals[0].TargetKind = "ACTOR"
			c.MemoryProposals[0].TargetID = c.Actions[0].TargetID
		}
		raw, e = json.Marshal(c)
		if e != nil {
			return e
		}
	}
	if (x.Fault == MockMalformedOriginalResult || x.Fault == MockUsagePendingRepair || x.Fault == MockCostFinalityPendingRepair) && d.Slot == 0 {
		raw = []byte(`{"dialogue":"我听到了。","actions":`)
	}
	if x.Fault == MockMalformedEveryResult {
		raw = []byte(`{"dialogue":"synthetic incomplete","actions":`)
	}
	if x.Fault == MockRefuseOriginalResult || x.Fault == MockUsagePendingRefusal {
		raw = []byte(`{"refusal":"synthetic policy refusal"}`)
	}
	body := string(raw)
	output := uint64(utf8.RuneCount(raw))
	phase = "output bounds"
	if output > m.OutputBound {
		return store.ErrDenied
	}
	stage, e := x.Files.AllocateDerived(id.Scope.ProductScope, body)
	if e != nil {
		return e
	}
	phase = "body binding"
	if x.Fault == MockDelayResultStage {
		select {
		case <-time.After(time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if e = x.Turns.Store.BindOriginalResultBody(ctx, d, x.Files.DerivedStorage(stage), stage.Hash); e != nil {
		// Provider completion and narrative retention are independent facts. A
		// withdrawn source may deny storing bytes without erasing original usage.
		if journalError := x.Journal.complete(ctx, completedMock(id, stage.Hash, "", input, output, false)); journalError != nil {
			return journalError
		}
		return e
	}
	phase = "body write"
	if e = x.Files.WriteDerived(ctx, id.Scope.ProductScope, stage, body); e != nil {
		return e
	}
	result := completedMock(id, stage.Hash, x.Files.DerivedStorage(stage), input, output, true)
	if x.Fault == MockCostFinalityPending || x.Fault == MockCostFinalityPendingRepair {
		// Complete real narrative and usage do not prove final supplier liability.
		// This fixed synthetic fault creates a nonfinal original journal bill.
		result.NoFurtherCost = false
	}
	if x.Fault == MockUsagePending || x.Fault == MockUsagePendingRepair || x.Fault == MockUsagePendingRefusal {
		// No reported measurement is different from an authoritative zero.
		// The complete narrative bytes remain the actual generated result.
		result.UsageKnown, result.NoFurtherCost = false, false
		result.InputUnits, result.OutputUnits = 0, 0
	}
	if e := x.Journal.complete(ctx, result); e != nil {
		return e
	}
	if !result.UsageKnown {
		return x.Journal.observeUsage(ctx, id, input, output)
	}
	return nil
}

func completedMock(id domain.MockRequestIdentity, hash, storage string, input, output uint64, available bool) domain.MockResultMetadata {
	return domain.MockResultMetadata{Identity: id, State: "SUCCEEDED", BodyHash: hash, BodyStorage: storage, InputUnits: input, OutputUnits: output, BodyAvailable: available, UsageKnown: true, NoFurtherCost: true, CostUnit: "MONEY", CostCurrency: "syntheticUSD", CostAtoms: 0}
}

// ExecuteRecovery consumes the fixed program decision and exact extra request.
// An existing marker is queried; it never receives another Generate permit.
func (x *Executor) ExecuteRecovery(ctx context.Context, lease domain.TaskLease, worker string) (err error) {
	phase := "recovery prepare"
	defer func() {
		if err != nil {
			err = fmt.Errorf("TURN %s: %w", phase, err)
		}
	}()
	if x == nil || x.Participant == nil {
		return store.ErrDenied
	}
	ctx, release, e := x.Participant.Enter(ctx, maintenance.HarnessWriter)
	if e != nil {
		return e
	}
	defer release()
	d, e := x.Turns.Store.RecoveryTurnDispatch(ctx, lease, worker)
	if errors.Is(e, sql.ErrNoRows) {
		d, e = x.Turns.Store.PrepareTurnRecovery(ctx, lease, worker)
	}
	if e != nil {
		return e
	}
	return x.executeRecoveryDispatch(ctx, lease, worker, d)
}

func (x *Executor) executeRecoveryDispatch(ctx context.Context, lease domain.TaskLease, worker string, d store.DispatchRequest) (err error) {
	phase := "extra resume"
	defer func() {
		if err != nil {
			err = fmt.Errorf("TURN %s: %w", phase, err)
		}
	}()
	if _, e := x.Turns.Store.QueryOriginalTurnCommit(ctx, *d.WorldScope, d.TaskID); e == nil {
		return nil
	} else if !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	id, e := x.Turns.Store.AttemptMockIdentity(ctx, *d.WorldScope, d.TaskID, d.AttemptID)
	if errors.Is(e, sql.ErrNoRows) {
		phase = "extra authorize"
		if _, e = x.Turns.Store.AuthorizeDispatch(ctx, d); e != nil {
			return e
		}
		d.CommandKey = content.Identity(d.AttemptID, "send")
		d.CommitKey = d.CommandKey
		phase = "extra marker"
		auth, e := x.Turns.Store.StartSend(ctx, d, d.AttemptID)
		if e != nil {
			return e
		}
		if !auth.CanSend || auth.OriginalSend() == nil {
			return store.ErrDenied
		}
		phase = "extra generate"
		if e = x.Generate(ctx, auth.OriginalSend()); e != nil {
			return e
		}
		if x.Fault == MockDropOriginalReply {
			return ErrMockReplyLost
		}
		if _, e = x.Turns.Store.ObserveOriginalReply(ctx, auth.OriginalSend()); e != nil {
			return e
		}
	} else if e != nil {
		return e
	} else {
		if _, e = x.Turns.Store.QueryAttemptValidation(ctx, *d.WorldScope, d.TaskID, d.AttemptID); errors.Is(e, sql.ErrNoRows) {
			phase = "extra original query"
			meta, e := x.originalResult(ctx, id)
			if e != nil {
				return e
			}
			if meta.State != "SUCCEEDED" {
				return store.ErrDenied
			}
		} else if e != nil {
			return e
		}
	}
	phase = "extra validation"
	if e = x.Turns.Store.ValidateAttemptTurn(ctx, lease, worker, d.AttemptID); e != nil {
		return e
	}
	code, e := x.Turns.Store.ReadAttemptValidation(ctx, domain.Scope{ProductScope: lease.ProductScope, WorldInstanceID: *lease.WorldInstanceID}, lease.TaskID, d.AttemptID)
	if e != nil {
		return e
	}
	if code == "REFUSAL" || code == "STRUCTURE_INVALID" {
		return x.Turns.Store.CommitRehearsalNegative(ctx, lease, worker, d.AttemptID)
	}
	phase = "extra approval"
	if e = x.Turns.Store.ApproveAttemptTurn(ctx, lease, worker, d.AttemptID); e != nil {
		return e
	}
	phase = "extra commit"
	_, e = x.Turns.Store.CommitAttemptTurn(ctx, lease, worker, d.AttemptID)
	return e
}

func (x *Executor) originalResult(ctx context.Context, id domain.MockRequestIdentity) (domain.MockCostMetadata, error) {
	known, e := x.Turns.Store.ReadOriginalQueryCompletion(ctx, id)
	if e == nil {
		return known, nil
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return domain.MockCostMetadata{}, e
	}
	q, e := x.Turns.Store.BeginOriginalQuery(ctx, id)
	if e != nil {
		return domain.MockCostMetadata{}, e
	}
	m, e := x.Turns.Store.RecordOriginalQuery(ctx, q)
	return domain.MockCostMetadata{Identity: m.Identity, State: m.State, UsageKnown: m.UsageKnown, NoFurtherCost: m.NoFurtherCost, CostAtoms: m.CostAtoms, CostUnit: m.CostUnit, CostCurrency: m.CostCurrency, InputUnits: m.InputUnits, OutputUnits: m.OutputUnits}, e
}

func (x *Executor) completeOriginal(ctx context.Context, lease domain.TaskLease, worker string) error {
	scope := domain.Scope{ProductScope: lease.ProductScope, WorldInstanceID: *lease.WorldInstanceID}
	if _, e := x.Turns.Store.QueryOriginalTurnCommit(ctx, scope, lease.TaskID); e == nil {
		return nil
	} else if !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	id, e := x.Turns.Store.OriginalMockIdentity(ctx, scope, lease.TaskID)
	if e != nil {
		return e
	}
	code, e := x.Turns.Store.ReadAttemptValidation(ctx, scope, lease.TaskID, id.AttemptID)
	if e != nil {
		return e
	}
	if code == "STRUCTURE_INVALID" {
		return x.ExecuteRecovery(ctx, lease, worker)
	}
	if code == "REFUSAL" {
		return x.Turns.Store.CommitRehearsalNegative(ctx, lease, worker, id.AttemptID)
	}
	if code != "STRUCTURE_VALID" {
		return store.ErrDenied
	}
	e = x.Turns.Store.ApproveAttemptTurn(ctx, lease, worker, id.AttemptID)
	if e == nil {
		_, e = x.Turns.Store.CommitAttemptTurn(ctx, lease, worker, id.AttemptID)
	}
	if e == nil {
		return nil
	}
	// The error is not authority: only the fixed full program check can issue a plan.
	d, recoveryError := x.Turns.Store.PrepareTurnRecovery(ctx, lease, worker)
	if recoveryError != nil {
		return errors.Join(e, recoveryError)
	}
	return x.executeRecoveryDispatch(ctx, lease, worker, d)
}

func (x *Executor) QueryNecessaryCostOnce(ctx context.Context) error {
	if x == nil || x.Participant == nil || x.Turns == nil {
		return store.ErrDenied
	}
	ctx, release, e := x.Participant.Enter(ctx, maintenance.QueryWriter)
	if e != nil {
		return e
	}
	defer release()
	id, e := x.Turns.Store.DueNecessaryCostQuery(ctx)
	if e != nil {
		return e
	}
	lease, e := x.Turns.Store.BeginNecessaryCostQuery(ctx, id)
	if e != nil {
		return e
	}
	_, e = x.Turns.Store.RecordNecessaryCostQuery(ctx, lease)
	return e
}

// DeliverLateMockBills is a closed internal fault fixture, absent from public
// routes, quotes, GUI settings and model parameters. No real supplier is used.
func (x *Executor) DeliverLateMockBills(ctx context.Context, id domain.MockRequestIdentity) error {
	if x == nil || x.Journal == nil {
		return store.ErrDenied
	}
	switch x.Fault {
	case MockLateTwoBillItems:
		if e := x.Journal.lateFaultBill(ctx, id, "late-a", 1, false); e != nil {
			return e
		}
		return x.Journal.lateFaultBill(ctx, id, "late-b", 2, false)
	case MockLateConflictingBillItem:
		return x.Journal.lateFaultBill(ctx, id, "late-a", 4, false)
	case MockFault(19):
		return x.Journal.publishObservedUsage(ctx, id)
	case MockLateFinalZeroBill:
		return x.Journal.lateFaultBill(ctx, id, "late-final", 0, true)
	default:
		return store.ErrDenied
	}
}
