package harness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/maintenance"
	store "loreviva.local/loreviva/internal/store/mysql"
	"strings"
	"time"
	"unicode/utf8"
)

func (x *Executor) GenerateDraftCandidate(ctx context.Context, p *store.OriginalSendPermit) error {
	if x == nil || x.Turns == nil || x.Files == nil || x.Journal == nil || x.Participant == nil {
		return store.ErrDenied
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ctx, release, e := x.Participant.Enter(ctx, maintenance.MockWriter)
	if e != nil {
		return e
	}
	defer release()
	d, m, e := p.TakeCandidate(ctx, x.Turns.Store)
	if e != nil {
		return e
	}
	h := sha256.Sum256(m.Canonical)
	input := uint64(utf8.RuneCount(m.Canonical))
	if !utf8.Valid(m.Canonical) || m.Connection != content.MockConnection || m.Model != content.MockModel || hex.EncodeToString(h[:]) != m.RequestHash || input == 0 || input > m.InputBound || m.InputBound > store.MockInputLimit || m.OutputBound == 0 || m.OutputBound > store.MockOutputLimit {
		return store.ErrDenied
	}
	var prompt domain.DraftCandidatePrompt
	if json.Unmarshal(m.Canonical, &prompt) != nil || prompt.Selection.ModuleID != m.Selection.ModuleID || len(prompt.Values) != len(m.Selection.Fields) {
		return store.ErrDenied
	}
	// Each supported text module executes this bounded real field handler. IDs and
	// selected values come only from the actual canonical request.
	fields := map[string]json.RawMessage{}
	for _, field := range m.Selection.Fields {
		value, ok := prompt.Values[field]
		if !ok {
			return store.ErrDenied
		}
		fields[field] = value
	}
	raw, e := json.Marshal(struct {
		Fields map[string]json.RawMessage `json:"fields"`
	}{fields})
	if e != nil {
		return e
	}
	if (x.Fault == MockMalformedOriginalResult || x.Fault == MockUsagePendingRepair || x.Fault == MockCostFinalityPendingRepair) && d.Slot == 0 {
		raw = []byte(`{"fields":`)
	}
	switch x.Fault {
	case MockRefuseOriginalResult:
		raw = []byte(`{"refusal":"synthetic provider refusal"}`)
	case MockDraftDuplicateField:
		raw = []byte(`{"fields":{"description":"a","description":"b"}}`)
	case MockDraftTrailingJSON:
		raw = append(raw, []byte(`{}`)...)
	case MockDraftUnselectedField:
		raw = []byte(`{"fields":{"description":"candidate","background":"unselected"}}`)
	case MockDraftInvalidFieldType:
		raw = []byte(`{"fields":{"description":[1]}}`)
	case MockDraftInvalidUTF8:
		raw = []byte{0xff, 0xfe, 0x7b, 0x7d}
	case MockDraftOverlongField:
		raw = []byte(`{"fields":{"description":"` + strings.Repeat("x", 2001) + `"}}`)
	}
	output := uint64(utf8.RuneCount(raw))
	if output == 0 || output > m.OutputBound {
		return store.ErrDenied
	}
	id := domain.MockRequestIdentity{Kind: "CANDIDATE", AuthorizationID: d.AuthorizationID, Scope: domain.Scope{ProductScope: d.ProductScope}, TaskID: d.TaskID, AttemptID: d.AttemptID, RequestKey: d.RequestKey, RequestHash: m.RequestHash, RouteID: d.Route, QuoteID: d.HarnessQuoteID, MarkerKey: d.CommitKey}
	if e = x.Journal.begin(ctx, id); e != nil {
		return e
	}
	bodyHash := sha256.Sum256(raw)
	storage, hash, e := x.Files.WriteCandidateResult(ctx, d, raw)
	if e != nil {
		if jerr := x.Journal.complete(ctx, completedMock(id, hex.EncodeToString(bodyHash[:]), "", input, output, false)); jerr != nil {
			return jerr
		}
		return e
	}
	result := completedMock(id, hash, storage, input, output, true)
	if x.Fault == MockCostFinalityPending || x.Fault == MockCostFinalityPendingRepair {
		result.NoFurtherCost = false
	}
	if x.Fault == MockUsagePending || x.Fault == MockUsagePendingRepair {
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
