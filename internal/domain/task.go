package domain

import (
	"errors"
	"time"
)

// TaskScope distinguishes administrative work from work in a running world.
// PRODUCT never invents a world identity; OwnerSubjectID is mandatory in both.
type TaskScope struct {
	ProductScope
	WorldScope     *Scope
	OwnerSubjectID string
}

func (s TaskScope) Validate() error {
	if err := s.ProductScope.Validate(); err != nil {
		return err
	}
	if err := ValidateID(s.OwnerSubjectID); err != nil {
		return err
	}
	if s.OwnerSubjectID != s.SubjectID {
		return errors.New("task owner must match authenticated subject")
	}
	if s.WorldScope != nil {
		if err := s.WorldScope.Validate(); err != nil {
			return err
		}
		if s.WorldScope.ProductScope != s.ProductScope {
			return errors.New("world scope does not match authenticated product scope")
		}
	}
	return nil
}

type TaskLease struct {
	ProductScope
	WorldInstanceID        *string
	OwnerSubjectID, TaskID string
	Fence                  uint64
	ExpiresAt              time.Time
}

// TaskIntake creates genuinely pending work, never a fabricated worker lease.
type TaskIntake struct {
	TaskScope
	TaskID          string
	NextRunAt       time.Time
	Kind, ProjectID string
}
