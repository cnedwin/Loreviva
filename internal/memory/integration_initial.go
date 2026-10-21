package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"loreviva.local/loreviva/internal/content"
	world "loreviva.local/loreviva/internal/runtime"
	store "loreviva.local/loreviva/internal/store/mysql"
	"reflect"
	"time"
)

func (s *MemoryService) IntegrationCreationStore() *store.Store { return s.Store }

// This import is admitted only by the exact Store creation seal. It creates
// initial evidence, never consent, subject permissions, or generation.use.
func (s *MemoryService) CreateIntegrationInitial(ctx context.Context, tx store.Tx, p *store.IntegrationCreationPlan, init world.Initialization) error {
	if e := s.Store.CheckIntegrationCreationTx(ctx, tx, p); e != nil {
		return e
	}
	n := Namespace{Scope: p.PlayerScope(), DataSubjectID: p.PlayerScope().SubjectID}
	if init.CreationCommandID != p.WorldID() || !reflect.DeepEqual(init.IntegrationInitialLineage, p.InitialLineage()) || !reflect.DeepEqual(init.Participants, []string{n.SubjectID}) {
		return store.ErrDenied
	}
	if e := tx.RequireLock(n.Key()); e != nil {
		return e
	}
	var exists bool
	if e := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM memory_aggregates WHERE tenant_id=? AND product_id=? AND world_instance_id=?)", n.TenantID, n.ProductID, n.WorldInstanceID).Scan(&exists); e != nil {
		return e
	}
	if exists {
		return store.ErrConflict
	}
	a := emptyAggregate(n)
	if e := populateInitial(ctx, tx, n, &a, init); e != nil {
		return e
	}
	if e := checkIntegrationLineage(a, p.InitialLineage(), n); e != nil {
		return e
	}
	return saveTx(ctx, tx, n, a)
}
func checkIntegrationLineage(a Aggregate, lineage *store.IntegrationInitialLineage, n Namespace) error {
	if lineage == nil || lineage.Policy != "INTEGRATION_INITIAL_PUBLISHED_PRIVATE_V1" || lineage.Player != n.SubjectID || lineage.World != n.WorldInstanceID {
		return store.ErrDenied
	}
	sources, nodes := map[string]bool{}, map[string]bool{}
	for _, proof := range lineage.Sources {
		if proof.Kind == "CORE" {
			continue
		}
		source, ok := a.Sources[proof.SourceID]
		if !ok {
			return store.ErrDenied
		}
		sum := sha256.Sum256([]byte(source.Body))
		if hex.EncodeToString(sum[:]) != proof.BodyHash || proof.Acquisition != lineage.CreationReceipt {
			return store.ErrDenied
		}
		id := content.Identity(proof.SourceID, proof.Actor, "initial")
		node, ok := a.Nodes[id]
		if !ok || node.Kind != proof.Kind || node.ActorID != proof.Actor || node.TargetID != n.SubjectID {
			return store.ErrDenied
		}
		sources[proof.SourceID] = true
		nodes[id] = true
	}
	if len(sources) != len(a.Sources) || len(nodes) != len(a.Nodes) {
		return store.ErrDenied
	}
	return nil
}
func (s *MemoryService) ValidateIntegrationInitial(ctx context.Context, tx store.Tx, p *store.IntegrationCreationPlan, init world.Initialization) error {
	if e := s.Store.CheckIntegrationCreationTx(ctx, tx, p); e != nil {
		return e
	}
	n := Namespace{Scope: p.PlayerScope(), DataSubjectID: p.PlayerScope().SubjectID}
	if e := tx.RequireLock(n.Key()); e != nil {
		return e
	}
	var raw []byte
	var rev uint64
	var count int
	if e := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM memory_aggregates WHERE tenant_id=? AND product_id=? AND world_instance_id=?", n.TenantID, n.ProductID, n.WorldInstanceID).Scan(&count); e != nil {
		return e
	}
	if count != 1 {
		return store.ErrDenied
	}
	if e := tx.QueryRowContext(ctx, "SELECT revision,document FROM memory_aggregates WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND data_subject_id=? AND aggregate_id=?", n.TenantID, n.ProductID, n.WorldInstanceID, n.SubjectID, n.Key().IDs[3]).Scan(&rev, &raw); e != nil {
		return e
	}
	if rev != 1 {
		return store.ErrDenied
	}
	var actual Aggregate
	if json.Unmarshal(raw, &actual) != nil {
		return store.ErrDenied
	}
	expected := emptyAggregate(n)
	if e := populateInitial(ctx, tx, n, &expected, init); e != nil {
		return e
	}
	// Only the original observed timestamp varies in this pure reconstruction;
	// every persisted source, acquisition, grant, node and policy stays exact.
	for id, v := range actual.Sources {
		if v.OccurredAt.IsZero() {
			return store.ErrDenied
		}
		v.OccurredAt = time.Time{}
		actual.Sources[id] = v
	}
	for id, v := range expected.Sources {
		v.OccurredAt = time.Time{}
		expected.Sources[id] = v
	}
	if !reflect.DeepEqual(actual, expected) {
		return store.ErrDenied
	}
	return checkIntegrationLineage(actual, p.InitialLineage(), n)
}
