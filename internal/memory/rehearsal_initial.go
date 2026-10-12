package memory

import (
	"context"
	"encoding/json"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	world "loreviva.local/loreviva/internal/runtime"
	store "loreviva.local/loreviva/internal/store/mysql"
	"strconv"
	"time"
)

// Synthetic INITIAL sources are created only inside the server-bound isolated
// initialization transaction. No private-generation permission is installed.
func (s *MemoryService) CreateRehearsalInitial(ctx context.Context, tx store.Tx, scope domain.Scope, root string, init world.Initialization, catalog map[string]generated.Release) error {
	if e := tx.AssertRehearsalInitialization(scope, root); e != nil {
		return e
	}
	if init.CreationCommandID != content.Identity(root, "rehearsal-init") || init.ReleaseID != content.Identity(root, "rehearsal-version") || len(init.Actors) == 0 {
		return ErrInvalid
	}
	n := Namespace{Scope: scope, DataSubjectID: scope.SubjectID}
	a, e := loadTx(ctx, tx, n)
	if e != nil {
		return e
	}
	if len(a.Sources) != 0 || len(a.Nodes) != 0 {
		return store.ErrConflict
	}
	var now time.Time
	if e = tx.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&now); e != nil {
		return e
	}
	add := func(actor, id, body, kind string) {
		if _, exists := a.Sources[id]; !exists {
			a.Intake++
			a.Sources[id] = Source{ID: id, Body: body, Domain: Sandbox, Audience: Private, Uses: []Use{PrivateResponse, Relationship}, Revision: 1, Epoch: a.Epoch, Intake: a.Intake, OccurredAt: now}
		}
		a.Grants = append(a.Grants, Grant{ActorID: actor, SourceID: id, Uses: []Use{PrivateResponse, Relationship}})
		a.Acquisitions = append(a.Acquisitions, Acquisition{ActorID: actor, SourceID: id, Kind: Initial, EvidenceID: init.CreationCommandID})
		node := content.Identity(id, actor, "initial")
		a.Nodes[node] = MemoryRecord{ID: node, ActorID: actor, TargetKind: "PLAYER", TargetID: scope.SubjectID, Kind: kind, Domain: Sandbox, Audience: Private, Uses: []Use{PrivateResponse, Relationship}, Sources: []string{id}, Body: body, Revision: 1}
	}
	for _, actor := range init.Actors {
		for index, goal := range append(append([]string{}, actor.OriginalSetup.InitialGoals...), actor.OriginalCharacter.LongTermGoals...) {
			add(actor.ID, content.Identity(init.CreationCommandID, actor.ID, "goal", strconv.Itoa(index)), goal, "GOAL")
		}
		var owner struct{ Kind, ReleaseID string }
		var ownerJSON struct {
			Kind      string `json:"kind"`
			ReleaseID string `json:"release_id"`
		}
		if e = json.Unmarshal(actor.OwnerReference, &ownerJSON); e != nil {
			return e
		}
		owner.Kind, owner.ReleaseID = ownerJSON.Kind, ownerJSON.ReleaseID
		characterRelease := init.ReleaseID
		if owner.Kind == "PINNED_DEFINITION" {
			characterRelease = owner.ReleaseID
		} else if owner.Kind != "LOCAL_DRAFT" {
			return ErrInvalid
		}
		type reference struct {
			ref     generated.DefinitionRef
			release string
		}
		refs := []reference{}
		for _, ref := range actor.OriginalSetup.KnowledgeRefs {
			refs = append(refs, reference{ref, init.ReleaseID})
		}
		for _, ref := range actor.OriginalCharacter.InitialKnowledgeRefs {
			refs = append(refs, reference{ref, characterRelease})
		}
		for _, entry := range refs {
			var ref struct {
				Kind      string `json:"kind"`
				ReleaseID string `json:"release_id"`
				ObjectID  string `json:"object_id"`
				Version   string `json:"version"`
				Hash      string `json:"content_hash"`
			}
			if e = json.Unmarshal(entry.ref, &ref); e != nil {
				return e
			}
			release := entry.release
			if ref.Kind == "PINNED_DEFINITION" {
				release = ref.ReleaseID
			} else if ref.Kind != "LOCAL_DRAFT" {
				return ErrInvalid
			}
			rel, ok := catalog[release]
			if !ok {
				return ErrInvalid
			}
			found := false
			for _, raw := range rel.Input.Objects {
				var head struct {
					ID string `json:"object_id"`
				}
				if json.Unmarshal(raw, &head) != nil {
					return ErrInvalid
				}
				if head.ID != ref.ObjectID {
					continue
				}
				var knowledge generated.KnowledgeDefinition
				if e = generated.Decode("KnowledgeDefinition", raw, &knowledge); e != nil {
					return e
				}
				hash, e := content.Hash(json.RawMessage(raw))
				if e != nil {
					return e
				}
				if ref.Kind == "PINNED_DEFINITION" && (hash != ref.Hash || string(knowledge.Version) != ref.Version) {
					return ErrInvalid
				}
				add(actor.ID, content.Identity(init.CreationCommandID, "knowledge", release, ref.ObjectID), knowledge.Text, "BELIEF")
				found = true
				break
			}
			if !found {
				return ErrInvalid
			}
		}
	}
	return saveTx(ctx, tx, n, a)
}
