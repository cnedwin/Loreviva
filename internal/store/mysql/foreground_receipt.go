package mysql

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"loreviva.local/loreviva/internal/domain"
)

type OriginalQueryReceipt struct {
	QueryKey, Kind                  string
	Ordinal                         int
	BeginCommitted, RecordCommitted bool
	Metadata                        domain.MockCostMetadata
}

func (l OriginalQueryLease) OriginalQueryKey() string {
	if l.cost {
		return ""
	}
	return l.queryKey
}

func originalQueryKey(id domain.MockRequestIdentity, kind string, ordinal int) string {
	return turnIdentity(id.AuthorizationID, "original-query", kind, fmt.Sprint(ordinal))
}
func originalQueryKind(reply bool) string {
	if reply {
		return "REPLY"
	}
	return "FOREGROUND"
}

func readOriginalQueryReceipt(ctx context.Context, tx *sql.Tx, id domain.MockRequestIdentity, key string) (OriginalQueryReceipt, uint64, error) {
	var out OriginalQueryReceipt
	var fence uint64
	var raw []byte
	e := tx.QueryRowContext(ctx, "SELECT kind,ordinal,query_key,lease_fence,metadata FROM harness_original_query_records WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=? AND query_key=?", id.Scope.TenantID, id.Scope.ProductID, id.TaskID, id.AttemptID, key).Scan(&out.Kind, &out.Ordinal, &out.QueryKey, &fence, &raw)
	if e != nil {
		return out, 0, e
	}
	if fence == 0 || (out.Kind != "REPLY" && out.Kind != "FOREGROUND") || (out.Kind == "REPLY" && out.Ordinal != 0) || (out.Kind == "FOREGROUND" && (out.Ordinal < 1 || out.Ordinal > 3)) || out.QueryKey != originalQueryKey(id, out.Kind, out.Ordinal) {
		return OriginalQueryReceipt{}, 0, ErrDenied
	}
	out.BeginCommitted = true
	if len(raw) > 0 {
		if json.Unmarshal(raw, &out.Metadata) != nil || out.Metadata.Identity != id || (out.Metadata.State != "UNKNOWN" && out.Metadata.State != "SUCCEEDED") {
			return OriginalQueryReceipt{}, 0, ErrDenied
		}
		out.RecordCommitted = true
	}
	return out, fence, nil
}

// Exact local outcome only. The cost-shaped projection cannot locate any body,
// mint an IO permit, prove candidate/world COMMIT, or reopen the original lease.
func (s *Store) QueryOriginalQueryReceipt(ctx context.Context, id domain.MockRequestIdentity, key string) (OriginalQueryReceipt, error) {
	if s.QueryAuthority == nil || id.Validate() != nil || domain.ValidateID(key) != nil {
		return OriginalQueryReceipt{}, ErrDenied
	}
	ctx, service, release, e := s.QueryAuthority.EnterOriginalQuery(ctx)
	if e != nil {
		return OriginalQueryReceipt{}, e
	}
	defer release()
	tx, e := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if e != nil {
		return OriginalQueryReceipt{}, e
	}
	defer tx.Rollback()
	if e = lockOriginalQuery(ctx, tx, id, service); e != nil {
		return OriginalQueryReceipt{}, e
	}
	out, _, e := readOriginalQueryReceipt(ctx, tx, id, key)
	return out, e
}

func validateOriginalQueryReceipt(ctx context.Context, tx *sql.Tx, lease OriginalQueryLease) error {
	r, f, e := readOriginalQueryReceipt(ctx, tx, lease.identity, lease.queryKey)
	if e != nil {
		return e
	}
	if f != lease.fence || r.Kind != originalQueryKind(lease.reply) || r.RecordCommitted {
		return ErrDenied
	}
	return nil
}

// Verify the fixed original sequence before allocating any further bounded
// command. A missing row is not permission to reset queries_used.
func validateOriginalQueryHistory(ctx context.Context, tx *sql.Tx, id domain.MockRequestIdentity, used int) error {
	rows, e := tx.QueryContext(ctx, "SELECT kind,ordinal,query_key,lease_fence,metadata FROM harness_original_query_records WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=? ORDER BY kind,ordinal", id.Scope.TenantID, id.Scope.ProductID, id.TaskID, id.AttemptID)
	if e != nil {
		return e
	}
	defer rows.Close()
	n, replies := 0, 0
	for rows.Next() {
		var kind, key string
		var ordinal int
		var fence uint64
		var raw []byte
		if e = rows.Scan(&kind, &ordinal, &key, &fence, &raw); e != nil {
			return e
		}
		if fence == 0 || key != originalQueryKey(id, kind, ordinal) {
			return ErrDenied
		}
		if len(raw) > 0 {
			var m domain.MockCostMetadata
			if json.Unmarshal(raw, &m) != nil || m.Identity != id || (m.State != "UNKNOWN" && m.State != "SUCCEEDED") {
				return ErrDenied
			}
		}
		if kind == "FOREGROUND" {
			n++
			if ordinal != n || n > 3 {
				return ErrDenied
			}
		} else if kind == "REPLY" {
			replies++
			if ordinal != 0 || replies > 1 {
				return ErrDenied
			}
		} else {
			return ErrDenied
		}
	}
	if e = rows.Err(); e != nil {
		return e
	}
	if n != used {
		return ErrDenied
	}
	return nil
}

func minimalOriginalMetadata(m domain.MockResultMetadata) domain.MockCostMetadata {
	return domain.MockCostMetadata{Identity: m.Identity, State: m.State, UsageKnown: m.UsageKnown, NoFurtherCost: m.NoFurtherCost, CostAtoms: m.CostAtoms, CostUnit: m.CostUnit, CostCurrency: m.CostCurrency, InputUnits: m.InputUnits, OutputUnits: m.OutputUnits}
}

// Unknown measurements are recorded as unknown, never authoritative zeros or
// quote estimates. This checks provider metadata, not narrative approval or
// service settlement eligibility.
func validOriginalUsageMetadata(m domain.MockCostMetadata) bool {
	if m.CostAtoms != 0 || m.CostUnit != "MONEY" || m.CostCurrency != "syntheticUSD" {
		return false
	}
	if !m.UsageKnown {
		// A later actual final-cost item can close supplier liability without
		// reporting units. Neither fact manufactures the other.
		return m.InputUnits == 0 && m.OutputUnits == 0
	}
	return m.InputUnits > 0 && m.InputUnits <= MockInputLimit && m.OutputUnits > 0 && m.OutputUnits <= MockOutputLimit
}

// Worker resumption reads a known recorded original result before contemplating
// another query. It still needs the separate owning current body/commit checks.
func (s *Store) ReadOriginalQueryCompletion(ctx context.Context, id domain.MockRequestIdentity) (domain.MockCostMetadata, error) {
	if s.QueryAuthority == nil || id.Validate() != nil {
		return domain.MockCostMetadata{}, ErrDenied
	}
	ctx, service, release, e := s.QueryAuthority.EnterOriginalQuery(ctx)
	if e != nil {
		return domain.MockCostMetadata{}, e
	}
	defer release()
	tx, e := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if e != nil {
		return domain.MockCostMetadata{}, e
	}
	defer tx.Rollback()
	if e = lockOriginalQuery(ctx, tx, id, service); e != nil {
		return domain.MockCostMetadata{}, e
	}
	var key string
	if e = tx.QueryRowContext(ctx, "SELECT query_key FROM harness_original_query_records WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=? AND JSON_UNQUOTE(JSON_EXTRACT(metadata,'$.State'))='SUCCEEDED' ORDER BY lease_fence DESC LIMIT 1", id.Scope.TenantID, id.Scope.ProductID, id.TaskID, id.AttemptID).Scan(&key); e != nil {
		return domain.MockCostMetadata{}, e
	}
	r, _, e := readOriginalQueryReceipt(ctx, tx, id, key)
	if e != nil {
		return domain.MockCostMetadata{}, e
	}
	if !r.RecordCommitted {
		return domain.MockCostMetadata{}, ErrDenied
	}
	return r.Metadata, nil
}
