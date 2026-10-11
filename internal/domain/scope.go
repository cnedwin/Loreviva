// Package domain defines internal authority scopes, not public API DTOs.
package domain

import (
	"errors"
	"regexp"
	"unicode/utf8"
)

var identityPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
var keyPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

type ProductScope struct{ TenantID, ProductID, SubjectID string }
type Scope struct {
	ProductScope
	WorldInstanceID string
}
type RelationshipScope struct {
	Scope
	ActorInstanceID, PlayerID string
}
type ActorRelationshipScope struct {
	Scope
	ActorInstanceID, TargetActorInstanceID string
}
type Versions struct {
	ReleaseID                                              string
	WorldRevision, ControlVersion, PrivacyEpoch, ViewEpoch uint64
}

func (v Versions) Validate() error { return ValidateID(v.ReleaseID) }

func ValidateID(id string) error {
	if !identityPattern.MatchString(id) {
		return errors.New("identity must be nonempty case-sensitive ASCII, at most 64 bytes")
	}
	return nil
}
func ValidateExternalIdentity(id string) error {
	if !utf8.ValidString(id) || utf8.RuneCountInString(id) < 1 || utf8.RuneCountInString(id) > 128 {
		return errors.New("external identity must be valid UTF-8 with 1 to 128 characters")
	}
	return nil
}
func ValidateIdempotencyKey(key string) error {
	if !keyPattern.MatchString(key) {
		return errors.New("idempotency key must be nonempty ASCII, at most 128 bytes")
	}
	return nil
}
func validateIDs(ids ...string) error {
	for _, id := range ids {
		if err := ValidateID(id); err != nil {
			return err
		}
	}
	return nil
}
func (s ProductScope) Validate() error { return validateIDs(s.TenantID, s.ProductID, s.SubjectID) }
func (s Scope) Validate() error {
	if err := s.ProductScope.Validate(); err != nil {
		return err
	}
	return ValidateID(s.WorldInstanceID)
}
func (s RelationshipScope) Validate() error {
	if err := s.Scope.Validate(); err != nil {
		return err
	}
	return validateIDs(s.ActorInstanceID, s.PlayerID)
}
func (s ActorRelationshipScope) Validate() error {
	if err := s.Scope.Validate(); err != nil {
		return err
	}
	return validateIDs(s.ActorInstanceID, s.TargetActorInstanceID)
}
