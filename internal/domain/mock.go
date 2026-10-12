package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
)

type MockHead struct {
	Sequence uint64
	Hash     string
}

func MockGenesis(installation, binding string) MockHead {
	raw, _ := json.Marshal([]string{"R0_MOCK_JOURNAL_V1", installation, binding})
	h := sha256.Sum256(raw)
	return MockHead{Hash: hex.EncodeToString(h[:])}
}

type MockRequestIdentity struct {
	AuthorizationID                                                         string
	Scope                                                                   Scope
	TaskID, AttemptID, RequestKey, RequestHash, RouteID, QuoteID, MarkerKey string
	// Empty is the immutable original WORLD encoding. Only the fixed Product
	// candidate owner emits CANDIDATE; it must never carry a world identity.
	Kind string `json:",omitempty"`
}

func (i MockRequestIdentity) Validate() error {
	switch i.Kind {
	case "":
		return i.Scope.Validate()
	case "CANDIDATE":
		if i.Scope.WorldInstanceID != "" {
			return errors.New("Product Mock identity cannot contain WORLD")
		}
		return i.Scope.ProductScope.Validate()
	default:
		return errors.New("unsupported Mock identity kind")
	}
}

type MockResultMetadata struct {
	UsageKnown, NoFurtherCost    bool
	CostAtoms                    int64
	CostUnit, CostCurrency       string
	Identity                     MockRequestIdentity
	State, BodyStorage, BodyHash string
	InputUnits, OutputUnits      uint64
	BodyAvailable                bool
}

type QueryServiceIdentity struct {
	Installation, ServiceID, Action, Policy string
	Generation                              uint64
}

// OriginalContextSnapshot freezes metadata at the actual send marker. It holds
// no body or standalone permission; current owning checks are always required.
type OriginalContextSnapshot struct {
	Recovery *RecoveryPlan `json:",omitempty"`
	World    WorldPerception
	Binding  GenerationBinding
	Versions Versions
	Sources  ResultSourceBinding
}

// RecoveryPlan is immutable server-generated metadata. The original result
// remains a governed real-byte dependency, never a plaintext copy in this row.
type RecoveryPlan struct {
	AttemptID, ApprovalID, Reason, OriginalPhase string
	OriginalRound                                uint64
	Original                                     MockResultMetadata
	BaseBinding, Binding                         GenerationBinding
}

// MockCostMetadata contains only original request and usage/cost evidence.
// It cannot address or authorize a result body.
type MockCostMetadata struct {
	Identity                  MockRequestIdentity
	State                     string
	UsageKnown, NoFurtherCost bool
	CostAtoms                 int64
	CostUnit, CostCurrency    string
	InputUnits, OutputUnits   uint64
}

// SettlementServiceIdentity is minted only by the current local DeadlineWriter.
type SettlementServiceIdentity struct {
	Installation, ServiceID, Action, Policy string
	Generation                              uint64
}

// MockBillItem is minimal independent provider accounting evidence, never body
// permission or a WORLD outcome. One cost is constrained by two supplier caps.
type MockBillItem struct {
	Identity                                  MockRequestIdentity
	Provider, Account, ItemID, Unit, Currency string
	Atoms                                     int64
	Final                                     bool
}
type OriginalBillEvidence interface {
	Item() MockBillItem
	Items() []MockBillItem
	Head() MockHead
	Close()
}

// BillScanReference locates a real immutable journal version. It grants no
// settlement capability and contains no body, price or customer authority.
type BillScanReference struct {
	Position       MockHead
	Identity       MockRequestIdentity
	ItemID, Digest string
}
type OriginalBillScan interface {
	Start() MockHead
	End() MockHead
	References() []BillScanReference
	Close()
}

// Original usage is independent provider metering, not a body or completion
// capability. The immutable original result can continue to say UNKNOWN.
type MockUsageMeasurement struct {
	Identity                MockRequestIdentity
	Provider, Account       string
	InputUnits, OutputUnits uint64
}
type OriginalUsageEvidence interface {
	Measurement() MockUsageMeasurement
	Head() MockHead
	Close()
}
