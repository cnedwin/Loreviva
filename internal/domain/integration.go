package domain

import "time"

type IntegrationKind string

const (
	IntegrationService IntegrationKind = "SERVICE"
	IntegrationClient  IntegrationKind = "CLIENT"
)

// Registry commands are private administrative inputs, not wire contracts or
// authority proofs. Exactly one closed branch must agree with Kind.
type RegistryCommand struct {
	Kind             string                   `json:"kind"`
	ProductID        string                   `json:"product_id"`
	ExpectedRevision uint64                   `json:"expected_revision"`
	TargetID         string                   `json:"target_id"`
	Customer         *RegisterCustomer        `json:"customer,omitempty"`
	Player           *RegisterPlayerAccess    `json:"player,omitempty"`
	Rotation         *RotateServiceKey        `json:"rotation,omitempty"`
	QualifiedTarget  *RegistryQualifiedTarget `json:"qualified_target,omitempty"`
}
type RegistryQualifiedTarget struct {
	CustomerID string `json:"customer_id"`
}
type RegisterCustomer struct {
	CustomerID       string    `json:"customer_id"`
	ServiceSubjectID string    `json:"service_subject_id"`
	ConsumerID       string    `json:"consumer_id"`
	SecretHandleRef  string    `json:"-"`
	ExpiresAt        time.Time `json:"expires_at"`
}
type RegisterPlayerAccess struct {
	CustomerID      string    `json:"customer_id"`
	ExternalIDBytes []byte    `json:"external_id"`
	PlayerSubjectID string    `json:"player_subject_id"`
	EntitlementID   string    `json:"entitlement_id"`
	ReleaseID       string    `json:"release_id"`
	Actions         []string  `json:"actions"`
	ExpiresAt       time.Time `json:"expires_at"`
}
type RotateServiceKey struct {
	CustomerID      string `json:"customer_id"`
	OldPrincipalID  string `json:"old_principal_id"`
	NewPrincipalID  string `json:"new_principal_id"`
	SecretHandleRef string `json:"-"`
}
type RegistrationExpiry struct {
	CustomerExpiresAt    time.Time  `json:"customer_expires_at"`
	ServiceKeyExpiresAt  *time.Time `json:"service_key_expires_at,omitempty"`
	MappingExpiresAt     *time.Time `json:"mapping_expires_at,omitempty"`
	EntitlementExpiresAt *time.Time `json:"entitlement_expires_at,omitempty"`
}
type RegistryReceipt struct {
	Operation          string              `json:"operation"`
	Key                string              `json:"key"`
	Hash               string              `json:"hash"`
	Target             string              `json:"target"`
	Revision           uint64              `json:"revision"`
	ReceiptRef         string              `json:"receipt_ref"`
	State              string              `json:"state"`
	CustomerID         string              `json:"customer_id,omitempty"`
	RegistrationExpiry *RegistrationExpiry `json:"registration_expiry,omitempty"`
}
type IntegrationPrincipal struct {
	Kind                                                                  IntegrationKind
	PrincipalID, Customer, Tenant, Product, StableSubject, World, Profile string
	ExpiresAt                                                             time.Time
}
