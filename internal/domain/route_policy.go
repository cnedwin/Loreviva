package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/wire"
)

const InstalledMockConnection = "mock-local"
const InstalledMockModel = "synthetic-text-v1"
const InstalledMockProvider = "r0-mock-provider"

// This is the descriptor of the compiled synthetic program already exposed by
// capabilities. It says nothing about a real provider, network probe or billing.
func InstalledMockDescriptorHash() string {
	raw, _ := json.Marshal(struct{ Connection, Model, Price string }{InstalledMockConnection, InstalledMockModel, "0"})
	raw, _ = wire.Canonical(raw)
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}
func MockModule(module string) bool {
	switch module {
	case "DEFAULT", "INTERACTION", "PROACTIVE", "AUTHORING", "KNOWLEDGE_EXTRACTION", "REHEARSAL", "EVALUATION":
		return true
	}
	return false
}

// Frozen server metadata for accepted work. It is never an execution permit.
// Nil on historical bindings means the original installation-only fallback.
type RoutePolicySnapshot struct {
	TenantRevision, ProductRevision uint64
	TenantDefaults, ProductDefaults []generated.StudioModuleDefault
	DescriptorHash                  string
}
