package app

import (
	"context"
	"encoding/json"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/memory"
	store "loreviva.local/loreviva/internal/store/mysql"
	"loreviva.local/loreviva/internal/wire"
	"net/http"
	"strconv"
	"time"
)

func memoryNamespace(s domain.ProductScope, q *http.Request) memory.Namespace {
	return memory.Namespace{Scope: domain.Scope{ProductScope: s, WorldInstanceID: q.PathValue("instance_id")}, DataSubjectID: q.URL.Query().Get("data_subject_id")}
}
func memoryWire(n memory.Namespace, a memory.Aggregate, v memory.MemoryRecord, epoch uint64) generated.Memory {
	out := generated.Memory{Scope: generated.WorldScope{TenantID: n.TenantID, ProductID: n.ProductID, WorldInstanceID: n.WorldInstanceID}, MemoryID: v.ID, ActorInstanceID: v.ActorID, TargetSubjectID: v.TargetID, MemoryKind: v.Kind, Content: v.Body, Revision: strconv.FormatUint(v.Revision, 10), PrivacyEpoch: strconv.FormatUint(epoch, 10), Validity: "ACTIVE", AllowedUses: []string{}, Sources: []generated.MemorySource{}, Audience: generated.Audience{Kind: "PRIVATE_RELATIONSHIP", SubjectIDS: []string{n.DataSubjectID}}}
	if v.Audience == memory.Public {
		out.Audience = generated.Audience{Kind: "PUBLIC", SubjectIDS: []string{}}
	}
	switch v.Kind {
	case "EPISODE", "PREFERENCE", "PROMISE", "RELATIONSHIP_INTERPRETATION", "BELIEF":
	default:
		out.MemoryKind = "BELIEF"
	}
	for _, u := range v.Uses {
		out.AllowedUses = append(out.AllowedUses, string(u))
	}
	for _, id := range v.Sources {
		source := a.Sources[id]
		kind := "PLAYER_STATEMENT"
		if source.Domain == memory.WorldFact {
			kind = "COMMITTED_EVENT"
		} else if source.Domain == memory.ActorBelief {
			kind = "KNOWLEDGE_VERSION"
		}
		var chosen *memory.Acquisition
		for _, acq := range a.Acquisitions {
			if acq.SourceID == id && acq.ActorID == v.ActorID {
				candidate := acq
				if chosen == nil || acq.EvidenceID == v.ID {
					chosen = &candidate
				}
				if acq.EvidenceID == v.ID {
					break
				}
			}
		}
		if chosen != nil {
			out.Sources = append(out.Sources, generated.MemorySource{SourceID: id, SourceKind: kind, SourceRevision: strconv.FormatUint(source.Revision, 10), AcquisitionKind: string(chosen.Kind), AcquiredByActorID: v.ActorID})
		}
	}
	return out
}
func governanceWire(v memory.GovernanceReceipt) generated.MemoryGovernanceStatusV1 {
	return generated.MemoryGovernanceStatusV1{TaskID: generated.Identity(v.TaskID), Blocking: v.Blocking, Preservation: v.Preservation, Cleanup: v.Cleanup, ExternalVerification: "NOT_APPLICABLE_LOCAL_ONLY", DeliveredCopies: "NOT_RECALLABLE"}
}
func parseMemoryCommand(raw []byte, key string) (memory.MemoryGovernanceCommand, error) {
	var v struct {
		Operation, Replacement string
		MemoryIDs              []string          `json:"memory_ids"`
		Expected               map[string]string `json:"expected_revisions"`
		Uses                   []memory.Use
		Selector               *struct {
			Kind       string
			TargetKind string `json:"target_kind"`
			IDs        []string
			Expected   map[string]string `json:"expected_revisions"`
			From, To   string
		}
	}
	if e := json.Unmarshal(raw, &v); e != nil {
		return memory.MemoryGovernanceCommand{}, wire.ErrJSON
	}
	c := memory.MemoryGovernanceCommand{Key: key, Action: v.Operation, Replacement: v.Replacement, Uses: v.Uses}
	expected := v.Expected
	c.Selector = memory.Selector{Kind: "IDS", TargetKind: "MEMORY_NODE", IDs: v.MemoryIDs, Expected: map[string]uint64{}}
	if v.Selector != nil {
		c.Selector.Kind = v.Selector.Kind
		c.Selector.TargetKind = v.Selector.TargetKind
		c.Selector.IDs = v.Selector.IDs
		expected = v.Selector.Expected
		if c.Selector.Kind == "TIME_RANGE" {
			var e error
			c.Selector.From, e = time.Parse(time.RFC3339Nano, v.Selector.From)
			if e != nil {
				return c, wire.ErrJSON
			}
			c.Selector.To, e = time.Parse(time.RFC3339Nano, v.Selector.To)
			if e != nil {
				return c, wire.ErrJSON
			}
			_, off := c.Selector.From.Zone()
			_, off2 := c.Selector.To.Zone()
			if off != 0 || off2 != 0 {
				return c, wire.ErrJSON
			}
			c.Selector.From = c.Selector.From.UTC()
			c.Selector.To = c.Selector.To.UTC()
		}
	}
	for id, v := range expected {
		x, e := strconv.ParseUint(v, 10, 64)
		if e != nil {
			return c, wire.ErrJSON
		}
		c.Selector.Expected[id] = x
	}
	return c, nil
}
func (r *Runtime) memoryRoutes(m *http.ServeMux) {
	root := "/world/v1/admin/instances/{instance_id}"
	m.HandleFunc("GET "+root+"/memories", r.admin("", "memory.metadata.read", func(ctx context.Context, s domain.ProductScope, q *http.Request, _ []byte) (int, any, error) {
		if r.Memory == nil {
			return 0, nil, store.ErrDenied
		}
		n := memoryNamespace(s, q)
		a, _, e := r.Memory.AdminRead(ctx, n, q.URL.Query().Get("actor_instance_id"), "memory.metadata.read")
		if e != nil {
			return 0, nil, e
		}
		out := generated.AdminMemoryMetadataPageV1{Scope: generated.WorldScope{TenantID: s.TenantID, ProductID: s.ProductID, WorldInstanceID: n.WorldInstanceID}, Items: []generated.AdminMemoryMetadataV1{}}
		for _, v := range a.Nodes {
			if v.ActorID == q.URL.Query().Get("actor_instance_id") {
				out.Items = append(out.Items, generated.AdminMemoryMetadataV1{MemoryID: generated.Identity(v.ID), Revision: generated.ExactUnsigned(strconv.FormatUint(v.Revision, 10)), ActorInstanceID: generated.Identity(v.ActorID), TargetSubjectID: generated.Identity(v.TargetID), MemoryKind: v.Kind, Blocked: v.Blocked})
			}
		}
		return 200, out, nil
	}))
	m.HandleFunc("GET "+root+"/memories/{memory_id}/body", r.admin("", "memory.private_body.read", func(ctx context.Context, s domain.ProductScope, q *http.Request, _ []byte) (int, any, error) {
		if r.Memory == nil {
			return 0, nil, store.ErrDenied
		}
		include := q.URL.Query().Get("include")
		if include != "" && include != "provenance_v1" {
			return 0, nil, wire.ErrJSON
		}
		n := memoryNamespace(s, q)
		actor := q.URL.Query().Get("actor_instance_id")
		a, epoch, e := r.Memory.AdminRead(ctx, n, actor, "memory.private_body.read")
		if e != nil {
			return 0, nil, e
		}
		for _, v := range a.Allowed(actor, memory.Private, memory.PrivateResponse, time.Now()) {
			if v.ID != q.PathValue("memory_id") || v.ActorID != actor {
				continue
			}
			body := memoryWire(n, a, v, epoch)
			if include == "" {
				return 200, body, nil
			}
			p := generated.MemoryProvenanceV1{Projection: "provenance_v1", Memory: body, EvidenceDomain: string(v.Domain), SourceEpochIDS: []generated.Identity{}, ParentMemoryIDS: identities(v.Parents)}
			for _, id := range v.Sources {
				p.SourceEpochIDS = append(p.SourceEpochIDS, generated.Identity(a.Sources[id].Epoch))
				p.OccurredAtEstimated = p.OccurredAtEstimated || a.Sources[id].EstimatedTime
			}
			return 200, p, nil
		}
		return 0, nil, content.ErrNotFound
	}))
	m.HandleFunc("POST "+root+"/memory-governance", r.admin("AdminMemoryGovernanceRequestV1", "memory.govern", func(ctx context.Context, s domain.ProductScope, q *http.Request, b []byte) (int, any, error) {
		if r.Memory == nil {
			return 0, nil, store.ErrDenied
		}
		v := decode[generated.AdminMemoryGovernanceRequestV1](b)
		c, e := parseMemoryCommand(v.Command, q.Header.Get("Idempotency-Key"))
		if e != nil {
			return 0, nil, e
		}
		if c.Action == "CORRECT" {
			if _, e = r.Auth.Authorize(ctx, q.Header.Get("Authorization"), s.ProductID, "memory.correct"); e != nil {
				return 0, nil, e
			}
		}
		scope := domain.RelationshipScope{Scope: domain.Scope{ProductScope: s, WorldInstanceID: q.PathValue("instance_id")}, ActorInstanceID: string(v.ActorInstanceID), PlayerID: string(v.DataSubjectID)}
		receipt, e := r.Memory.Govern(ctx, scope, c)
		return 202, governanceWire(receipt), e
	}))
	m.HandleFunc("POST "+root+"/memory-use-policy", r.admin("AdminMemoryUsePolicyRequestV1", "memory.policy", func(ctx context.Context, s domain.ProductScope, q *http.Request, b []byte) (int, any, error) {
		if r.Memory == nil {
			return 0, nil, store.ErrDenied
		}
		v := decode[generated.AdminMemoryUsePolicyRequestV1](b)
		revision, e := strconv.ParseUint(string(v.ExpectedPolicyRevision), 10, 64)
		if e != nil {
			return 0, nil, wire.ErrJSON
		}
		uses := []memory.Use{}
		for _, u := range v.Uses {
			uses = append(uses, memory.Use(u))
		}
		scope := domain.RelationshipScope{Scope: domain.Scope{ProductScope: s, WorldInstanceID: q.PathValue("instance_id")}, ActorInstanceID: string(v.ActorInstanceID), PlayerID: string(v.DataSubjectID)}
		receipt, e := r.Memory.Govern(ctx, scope, memory.MemoryGovernanceCommand{Key: q.Header.Get("Idempotency-Key"), Action: "SET_USE_POLICY", PolicyAction: v.Action, PolicyRevision: revision, Uses: uses})
		return 202, governanceWire(receipt), e
	}))
	m.HandleFunc("GET "+root+"/memory-governance/{task_id}", r.admin("", "memory.govern", func(ctx context.Context, s domain.ProductScope, q *http.Request, _ []byte) (int, any, error) {
		if r.Memory == nil {
			return 0, nil, store.ErrDenied
		}
		v, e := r.Memory.Status(ctx, memoryNamespace(s, q), q.PathValue("task_id"))
		return 200, governanceWire(v), e
	}))
}

func identities(ids []string) []generated.Identity {
	out := []generated.Identity{}
	for _, id := range ids {
		out = append(out, generated.Identity(id))
	}
	return out
}

func (r *Runtime) originalMemoryRecovery(ctx context.Context, s domain.ProductScope, q *http.Request, raw []byte, schema string) bool {
	if r.Memory == nil {
		return false
	}
	var command memory.MemoryGovernanceCommand
	actor, subject := "", ""
	if schema == "AdminMemoryGovernanceRequestV1" {
		var v generated.AdminMemoryGovernanceRequestV1
		if json.Unmarshal(raw, &v) != nil {
			return false
		}
		var e error
		command, e = parseMemoryCommand(v.Command, q.Header.Get("Idempotency-Key"))
		if e != nil {
			return false
		}
		actor, subject = string(v.ActorInstanceID), string(v.DataSubjectID)
	} else {
		var v generated.AdminMemoryUsePolicyRequestV1
		if json.Unmarshal(raw, &v) != nil {
			return false
		}
		revision, e := strconv.ParseUint(string(v.ExpectedPolicyRevision), 10, 64)
		if e != nil {
			return false
		}
		command = memory.MemoryGovernanceCommand{Key: q.Header.Get("Idempotency-Key"), Action: "SET_USE_POLICY", PolicyAction: v.Action, PolicyRevision: revision, Uses: []memory.Use{}}
		for _, u := range v.Uses {
			command.Uses = append(command.Uses, memory.Use(u))
		}
		actor, subject = string(v.ActorInstanceID), string(v.DataSubjectID)
	}
	scope := domain.RelationshipScope{Scope: domain.Scope{ProductScope: s, WorldInstanceID: q.PathValue("instance_id")}, ActorInstanceID: actor, PlayerID: subject}
	return r.Memory.MatchesOriginalGovernance(ctx, scope, command)
}
