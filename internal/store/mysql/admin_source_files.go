package mysql

import (
	"context"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/domain"
	"sort"
)

// Internal server-derived references. These are never public DTOs or journal
// fields, and cannot be exchanged for arbitrary path or file read permission.
type CandidateSourceAsset struct{ AssetID, Storage, Hash string }
type CandidateSourceEvidence interface{ Close() }

func candidateSourceAssetsTx(ctx context.Context, u *unit, s domain.ProductScope, m domain.DraftCandidateMaterial) ([]CandidateSourceAsset, error) {
	out := []CandidateSourceAsset{}
	seen := map[string]bool{}
	storageHashes := map[string]string{}
	for _, node := range m.Sources {
		if node.AssetID == "" || seen[node.AssetID] {
			continue
		}
		seen[node.AssetID] = true
		if e := u.RequireLock(LockKey{Rank: 90, Kind: "asset", IDs: []string{s.TenantID, s.ProductID, node.AssetID}}); e != nil {
			return nil, e
		}
		var storage string
		if e := u.tx.QueryRowContext(ctx, "SELECT storage_key FROM assets WHERE tenant_id=? AND product_id=? AND asset_id=? AND state='READY'", s.TenantID, s.ProductID, node.AssetID).Scan(&storage); e != nil {
			return nil, e
		}
		if len(mustHash(storage)) != 16 || len(mustHash(node.AssetHash)) != 32 {
			return nil, ErrDenied
		}
		if prior, ok := storageHashes[storage]; ok {
			if prior != node.AssetHash {
				return nil, ErrDenied
			}
			continue
		}
		storageHashes[storage] = node.AssetHash
		out = append(out, CandidateSourceAsset{node.AssetID, storage, node.AssetHash})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Storage < out[j].Storage })
	return out, nil
}

// Fixed current-generation source qualification, used only by the registered
// Files owner before intake/send/result transactions. Terminal reads use S55.
func (s *Store) InspectCandidateSourceMaterial(ctx context.Context, scope domain.ProductScope, m domain.DraftCandidateMaterial) ([]CandidateSourceAsset, error) {
	if s.Guard == nil || s.DraftCandidateOwner == nil || len(m.Sources) == 0 || len(m.SourceHash) != 64 {
		return nil, ErrDenied
	}
	ctx, release, e := s.EnterOperation(ctx)
	if e != nil {
		return nil, e
	}
	defer release()
	g, e := s.candidateGates(ctx, scope)
	if e != nil {
		return nil, e
	}
	keys, e := s.DraftCandidateOwner.PlanCandidateLocks(ctx, scope, m.Selection)
	if e != nil {
		return nil, e
	}
	m.Canonical = nil
	key := turnIdentity(scope.TenantID, scope.ProductID, scope.SubjectID, m.Selection.DraftID, m.RequestHash, "source-inspect")
	r := BarrierRequest{ProductScope: scope, OwnerSubjectID: scope.SubjectID, Operation: "CANDIDATE_SOURCE_INSPECT", CommandKey: key, CommitKey: key, GateVersions: g, Locks: keys, commandOnly: true}
	inspected := errors.New("candidate sources inspected without mutation")
	var out []CandidateSourceAsset
	_, _, e = s.barrier(ctx, r, func(tx Tx) error {
		u := tx.(*unit)
		u.routePolicyFrozen = true
		u.routePolicy = m.RoutePolicy
		current, e := s.DraftCandidateOwner.BuildCandidateTx(ctx, tx, scope, m.Selection)
		if e != nil {
			return e
		}
		current.Canonical = nil
		if !sameCandidateBinding(current, m) {
			return ErrDenied
		}
		out, e = candidateSourceAssetsTx(ctx, tx.(*unit), scope, m)
		if e != nil {
			return e
		}
		return inspected
	})
	if !errors.Is(e, inspected) {
		if e == nil {
			e = ErrDenied
		}
		return nil, e
	}
	return out, nil
}

func (s *Store) candidateOriginalSources(ctx context.Context, d DispatchRequest) (CandidateSourceEvidence, error) {
	if s.DraftCandidateReader == nil || !d.candidateExecution || d.WorldScope != nil {
		return nil, ErrDenied
	}
	var raw []byte
	var m domain.DraftCandidateMaterial
	if e := s.DB.QueryRowContext(ctx, "SELECT binding FROM harness_admin_candidates WHERE tenant_id=? AND product_id=? AND task_id=? AND owner_subject_id=?", d.ProductScope.TenantID, d.ProductScope.ProductID, d.TaskID, d.OwnerSubjectID).Scan(&raw); e != nil {
		return nil, e
	}
	if json.Unmarshal(raw, &m) != nil {
		return nil, ErrDenied
	}
	return s.DraftCandidateReader.ReadCandidateSourceEvidence(ctx, d.ProductScope, m)
}

// No caller-supplied source lists: result byte owners can only reacquire the
// original binding of a currently owned fixed candidate dispatch.
func (s *Store) CandidateOriginalSourceEvidence(ctx context.Context, d DispatchRequest) (CandidateSourceEvidence, error) {
	return s.candidateOriginalSources(ctx, d)
}
