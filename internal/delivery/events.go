package delivery

import (
	"context"
	"errors"
	"loreviva.local/loreviva/internal/generated"
	store "loreviva.local/loreviva/internal/store/mysql"
)

func (s *Service) events(ctx context.Context, world, cursor string, limit uint16) (generated.IntegrationEventPage, error) {
	return retry(func() (generated.IntegrationEventPage, error) {
		before, e := s.store.BeginIntegrationPage(ctx, world, cursor, limit)
		if e != nil {
			return generated.IntegrationEventPage{}, e
		}
		defer before.Close()
		page, e := before.EventPage(cursor, limit)
		if e != nil {
			return generated.IntegrationEventPage{}, e
		}
		refs, e := before.PacketReferences()
		if e != nil {
			return generated.IntegrationEventPage{}, e
		}
		if e = before.Finish(ctx); e != nil {
			return generated.IntegrationEventPage{}, e
		}
		for _, ref := range refs {
			if !ref.Available {
				raw, e := packetDelivery(ref, generated.IntegrationApprovedPacket{})
				if e != nil {
					return generated.IntegrationEventPage{}, e
				}
				page.Deliveries = append(page.Deliveries, raw)
				continue
			}
			scope, e := s.store.IntegrationTurnScope(ctx, ref.TaskID)
			if e != nil || scope.WorldInstanceID != world {
				return generated.IntegrationEventPage{}, store.ErrDenied
			}
			view, e := s.turns.GetTurn(ctx, scope, ref.TaskID)
			if e != nil {
				return generated.IntegrationEventPage{}, e
			}
			if len(view.ApprovedPackets) != 1 {
				return generated.IntegrationEventPage{}, store.ErrDenied
			}
			packet, e := s.mediaPacket(ctx, ref, view.ApprovedPackets[0])
			if errors.Is(e, errMediaUnavailable) {
				ref.Available = false
				e = nil
			}
			if e != nil {
				return generated.IntegrationEventPage{}, e
			}
			raw, e := packetDelivery(ref, packet)
			if e != nil {
				return generated.IntegrationEventPage{}, e
			}
			page.Deliveries = append(page.Deliveries, raw)
		}
		// All packet IO is over. Recheck the complete page, not separate success
		// flags from earlier packet reads, before releasing any pending output.
		after, e := s.store.BeginIntegrationPage(ctx, world, cursor, limit)
		if e != nil {
			return generated.IntegrationEventPage{}, e
		}
		defer after.Close()
		if e = after.MatchFinished(before); e != nil {
			return generated.IntegrationEventPage{}, e
		}
		if e = after.Finish(ctx); e != nil {
			return generated.IntegrationEventPage{}, e
		}
		_, e = checked("IntegrationEventPage", page)
		return page, e
	})
}
func (s *Service) GetEvents(ctx context.Context, world, cursor string) (generated.IntegrationEventPage, error) {
	return s.GetEventsPage(ctx, world, cursor, 50)
}
func (s *Service) GetEventsPage(ctx context.Context, world, cursor string, limit uint16) (generated.IntegrationEventPage, error) {
	ctx, done, e := s.enter(ctx)
	if e != nil {
		return generated.IntegrationEventPage{}, e
	}
	defer done()
	return s.events(ctx, world, cursor, limit)
}
func (s *Service) WriteEvents(ctx context.Context, world, cursor, profile string, limit uint16, write func([]byte) error) error {
	ctx, done, e := s.enter(ctx)
	if e != nil {
		return e
	}
	defer done()
	v, e := s.events(ctx, world, cursor, limit)
	if e != nil {
		return e
	}
	raw, e := checked("IntegrationEventPage", v)
	if e != nil {
		return e
	}
	if profile == "public_v1" {
		raw, e = publicMediaProjection(raw)
		if e != nil {
			return e
		}
		if e = generated.Validate("EventPage", raw); e != nil {
			return e
		}
	} else if profile != "integration_r0_v1" {
		return store.ErrDenied
	}
	return boundedWrite(ctx, write, raw)
}
