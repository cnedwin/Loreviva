package harness

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/memory"
	store "loreviva.local/loreviva/internal/store/mysql"
	"strconv"
)

type TurnService struct{ Store *store.Store }

func NewTurnService(s *store.Store) *TurnService { return &TurnService{Store: s} }
func (s *TurnService) command(ctx context.Context, scope domain.Scope, actor, key string) (store.CommandBarrierRequest, error) {
	var out store.CommandBarrierRequest
	if scope.Validate() != nil || domain.ValidateID(actor) != nil || domain.ValidateIdempotencyKey(key) != nil {
		return out, store.ErrDenied
	}
	if c, handled, e := s.Store.IntegrationWorkerCommand(ctx, scope, key); handled {
		return c, e
	}
	g, e := content.Gate(ctx, s.Store, scope.ProductScope)
	if e != nil {
		return out, e
	}
	var v domain.Versions
	e = s.Store.DB.QueryRowContext(ctx, "SELECT w.release_id,w.world_revision,w.control_version,w.privacy_epoch,w.view_epoch FROM world_controls w JOIN runtime_instances i ON i.tenant_id=w.tenant_id AND i.product_id=w.product_id AND i.world_instance_id=w.world_instance_id WHERE w.tenant_id=? AND w.product_id=? AND w.world_instance_id=? AND i.owner_subject_id=?", scope.TenantID, scope.ProductID, scope.WorldInstanceID, scope.SubjectID).Scan(&v.ReleaseID, &v.WorldRevision, &v.ControlVersion, &v.PrivacyEpoch, &v.ViewEpoch)
	if e != nil {
		return out, e
	}
	locks := memory.GenerationContextLocks(memory.ActorGenerationRequest{Scope: scope, ActorID: actor, DataSubjectID: scope.SubjectID, TargetKind: "PLAYER", TargetID: scope.SubjectID})
	return store.CommandBarrierRequest{ProductScope: scope.ProductScope, WorldScope: &scope, Versions: &v, OwnerSubjectID: scope.SubjectID, CommandKey: key, GateVersions: g, Locks: locks}, nil
}
func (s *TurnService) QuoteTurn(ctx context.Context, scope domain.Scope, request generated.QuoteRequest, key string) (generated.Quote, error) {
	ctx, release, e := s.Store.EnterOperation(ctx)
	if e != nil {
		return generated.Quote{}, e
	}
	defer release()
	if request.PrimaryActorID == nil {
		return generated.Quote{}, store.ErrDenied
	}
	c, e := s.command(ctx, scope, *request.PrimaryActorID, key)
	if e != nil {
		return generated.Quote{}, e
	}
	return s.Store.QuoteTurn(ctx, c, request)
}
func (s *TurnService) SubmitTurn(ctx context.Context, scope domain.Scope, request generated.TurnRequest, key string) (generated.TurnAccepted, error) {
	ctx, release, e := s.Store.EnterOperation(ctx)
	if e != nil {
		return generated.TurnAccepted{}, e
	}
	defer release()
	hash, e := store.TurnCommandHash(scope, request)
	if e != nil {
		return generated.TurnAccepted{}, e
	}
	if original, e := s.Store.OriginalTurn(ctx, scope, key, hash); e == nil {
		return original, nil
	} else if !errors.Is(e, sql.ErrNoRows) {
		return generated.TurnAccepted{}, e
	}
	c, e := s.command(ctx, scope, request.PrimaryActorID, key)
	if e != nil {
		return generated.TurnAccepted{}, e
	}
	if request.ExpectedVersions.WorldRevision != strconv.FormatUint(c.Versions.WorldRevision, 10) || request.ExpectedVersions.ControlVersion != strconv.FormatUint(c.Versions.ControlVersion, 10) {
		return generated.TurnAccepted{}, store.ErrConflict
	}
	return s.Store.AcceptR0Turn(ctx, c, request)
}
func (s *TurnService) GetTurn(ctx context.Context, scope domain.Scope, id string) (generated.TurnView, error) {
	ctx, release, e := s.Store.EnterOperation(ctx)
	if e != nil {
		return generated.TurnView{}, e
	}
	defer release()
	out, e := s.Store.ReadTurn(ctx, scope, id)
	if e != nil || out.Task.TaskState != "COMMITTED" {
		return out, e
	}
	if s.Store.ApprovedTurnReader == nil {
		return generated.TurnView{}, store.ErrDenied
	}
	body, commit, e := s.Store.ApprovedTurnReader.ReadApprovedTurn(ctx, scope, id)
	if e != nil {
		return generated.TurnView{}, e
	}
	var candidate Candidate
	if json.Unmarshal(body, &candidate) != nil {
		return generated.TurnView{}, store.ErrDenied
	}
	var actor string
	if e = s.Store.DB.QueryRowContext(ctx, "SELECT actor_id FROM harness_turns WHERE tenant_id=? AND product_id=? AND task_id=? AND owner_subject_id=?", scope.TenantID, scope.ProductID, id, scope.SubjectID).Scan(&actor); e != nil {
		return generated.TurnView{}, e
	}
	kind := "DIALOGUE"
	if len(candidate.Actions) > 0 {
		kind = "OPERATION_RESULT"
		if len(commit.EventIDs) == 0 {
			return generated.TurnView{}, store.ErrDenied
		}
	}
	segment, _ := json.Marshal(map[string]any{"segment_id": content.Identity(commit.ResultID, "segment"), "speaker_actor_id": actor, "kind": kind, "text": candidate.Dialogue, "committed_event_ids": commit.EventIDs})
	packet := generated.ApprovedPacket{PacketID: commit.ResultID, TurnID: id, Scope: out.Task.Scope, Versions: generated.Versions{ReleaseID: commit.Versions.ReleaseID, WorldRevision: strconv.FormatUint(commit.Versions.WorldRevision, 10), ControlVersion: strconv.FormatUint(commit.Versions.ControlVersion, 10), PrivacyEpoch: strconv.FormatUint(commit.Versions.PrivacyEpoch, 10), ViewEpoch: strconv.FormatUint(commit.Versions.ViewEpoch, 10)}, Audience: generated.Audience{Kind: "PRIVATE_RELATIONSHIP", SubjectIDS: []string{scope.SubjectID}}, TextSegments: []generated.TextSegment{segment}, Actions: []generated.ApprovedAction{}, Media: []generated.MediaRef{}}
	for i, a := range candidate.Actions {
		params := map[string]json.RawMessage{}
		params["item_id"], _ = json.Marshal(a.ItemID)
		packet.Actions = append(packet.Actions, generated.ApprovedAction{ActionID: content.Identity(commit.ResultID, "action", strconv.Itoa(i)), Kind: a.Kind, CommittedEventIDS: commit.EventIDs, Parameters: params})
	}
	out.ApprovedPackets = []generated.ApprovedPacket{packet}
	raw, e := json.Marshal(out)
	if e != nil {
		return generated.TurnView{}, e
	}
	if e = generated.Decode("TurnView", raw, &out); e != nil {
		return generated.TurnView{}, e
	}
	return out, nil
}

// PrepareOriginalTurn is the fixed internal worker boundary. Caller-supplied
// routes, account amounts, prompt hashes and provider keys are not accepted.
func (s *TurnService) PrepareOriginalTurn(ctx context.Context, lease domain.TaskLease, worker string) (store.DispatchRequest, error) {
	var issuerError error
	ctx, issuerError = s.Store.EnterIntegrationTaskIssuer(ctx, lease, worker)
	if issuerError != nil {
		return store.DispatchRequest{}, issuerError
	}
	if d, e := s.Store.PrepareCurrentRehearsalTurn(ctx, lease, worker); e != nil {
		return store.DispatchRequest{}, e
	} else if d != nil {
		return *d, nil
	}
	ctx, release, e := s.Store.EnterOperation(ctx)
	if e != nil {
		return store.DispatchRequest{}, e
	}
	defer release()
	if lease.WorldInstanceID == nil || lease.SubjectID != lease.OwnerSubjectID {
		return store.DispatchRequest{}, store.ErrDenied
	}
	scope := domain.Scope{ProductScope: lease.ProductScope, WorldInstanceID: *lease.WorldInstanceID}
	var actor string
	var raw []byte
	if e = s.Store.DB.QueryRowContext(ctx, "SELECT actor_id,versions FROM harness_turns WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND owner_subject_id=? AND task_id=?", scope.TenantID, scope.ProductID, scope.WorldInstanceID, scope.SubjectID, lease.TaskID).Scan(&actor, &raw); e != nil {
		return store.DispatchRequest{}, e
	}
	c, e := s.command(ctx, scope, actor, content.Identity(lease.TaskID, "context"))
	if e != nil {
		return store.DispatchRequest{}, e
	}
	var original domain.Versions
	if e = json.Unmarshal(raw, &original); e != nil {
		return store.DispatchRequest{}, e
	}
	r := store.BarrierRequest{ProductScope: scope.ProductScope, WorldScope: &scope, Versions: &original, OwnerSubjectID: scope.SubjectID, TaskID: lease.TaskID, WorkerID: worker, Fence: lease.Fence, GateVersions: c.GateVersions, Operation: "HARNESS_CONTEXT", CommandKey: c.CommandKey, CommitKey: c.CommandKey, Locks: c.Locks}
	if _, e = s.Store.InitializeHarnessContext(ctx, r, domain.GenerationSelection{ActorID: actor, DataSubjectID: scope.SubjectID, TargetKind: "PLAYER", TargetID: scope.SubjectID}); e != nil {
		return store.DispatchRequest{}, e
	}
	return s.Store.OriginalTurnDispatch(ctx, r)
}
