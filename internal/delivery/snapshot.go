package delivery

import (
	"context"
	"loreviva.local/loreviva/internal/generated"
)

func (s *Service) snapshot(ctx context.Context, world string) (generated.Snapshot, error) {
	return retry(func() (generated.Snapshot, error) {
		h, e := s.store.BeginIntegrationRead(ctx, world, "SNAPSHOT")
		if e != nil {
			return generated.Snapshot{}, e
		}
		defer h.Close()
		v, e := h.Snapshot()
		if e != nil {
			return generated.Snapshot{}, e
		}
		if e = h.Finish(ctx); e != nil {
			return generated.Snapshot{}, e
		}
		if _, e = checked("Snapshot", v); e != nil {
			return generated.Snapshot{}, e
		}
		return v, nil
	})
}
func (s *Service) GetSnapshot(ctx context.Context, world string) (generated.Snapshot, error) {
	ctx, done, e := s.enter(ctx)
	if e != nil {
		return generated.Snapshot{}, e
	}
	defer done()
	return s.snapshot(ctx, world)
}
func (s *Service) WriteSnapshot(ctx context.Context, world string, write func([]byte) error) error {
	ctx, done, e := s.enter(ctx)
	if e != nil {
		return e
	}
	defer done()
	v, e := s.snapshot(ctx, world)
	if e != nil {
		return e
	}
	raw, e := checked("Snapshot", v)
	if e != nil {
		return e
	}
	return boundedWrite(ctx, write, raw)
}
