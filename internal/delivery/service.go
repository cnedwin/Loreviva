// Package delivery exposes only current, filtered integration projections.
package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/files"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/harness"
	"loreviva.local/loreviva/internal/maintenance"
	store "loreviva.local/loreviva/internal/store/mysql"
	"time"
)

type Service struct {
	store       *store.Store
	turns       *harness.TurnService
	participant *maintenance.Participant
	owner       store.DeliverySnapshotOwner
	files       *files.Store
}

func (s *Service) DeliveryReaderStore() *store.Store {
	if s == nil {
		return nil
	}
	return s.store
}

// Assembly validates actual object identities. A same-type Guard, foreign Files
// or independently constructed owner never becomes a delivery capability.
func New(db *store.Store, turns *harness.TurnService, p *maintenance.Participant, owner store.DeliverySnapshotOwner) (*Service, error) {
	if db == nil || turns == nil || turns.Store != db || p == nil || db.Guard != p || owner == nil || owner.DeliverySnapshotStore() != db {
		return nil, store.ErrDenied
	}
	f, ok := db.ApprovedTurnReader.(*files.Store)
	if !ok || f == nil || f.DB != db || db.OriginalResultReader != f {
		return nil, store.ErrDenied
	}
	s := &Service{db, turns, p, owner, f}
	if e := db.BindDeliveryReader(s, p, owner); e != nil {
		return nil, e
	}
	if e := db.BindRuntimeMediaOwner(s, f); e != nil {
		return nil, e
	}
	return s, nil
}
func (s *Service) enter(ctx context.Context) (context.Context, func(), error) {
	if s == nil || s.turns.Store != s.store || s.store.ApprovedTurnReader != s.files || s.store.OriginalResultReader != s.files || s.files.DB != s.store {
		return ctx, nil, store.ErrDenied
	}
	next, done, e := s.participant.EnterDelivery(ctx, s.store)
	if e != nil {
		return ctx, nil, e
	}
	if e = s.store.CheckDeliveryReader(next, s, s.owner); e != nil {
		done()
		return ctx, nil, e
	}
	return next, done, nil
}
func checked(schema string, v any) ([]byte, error) {
	raw, e := json.Marshal(v)
	if e != nil {
		return nil, e
	}
	if e = generated.Validate(schema, raw); e != nil {
		return nil, e
	}
	return raw, nil
}
func retry[T any](run func() (T, error)) (T, error) {
	var out T
	var e error
	for i := 0; i < 3; i++ {
		out, e = run()
		if !errors.Is(e, store.ErrReplan) {
			return out, e
		}
	}
	var zero T
	return zero, store.ErrReplan
}

// The callback receives validated bytes only. SQL/read handles have already
// closed; the real API parent remains held only for this bounded write.
func boundedWrite(ctx context.Context, write func([]byte) error, raw []byte) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	start := time.Now()
	e := write(append([]byte(nil), raw...))
	if e != nil {
		return e
	}
	if time.Since(start) > 5*time.Second {
		return context.DeadlineExceeded
	}
	return ctx.Err()
}
