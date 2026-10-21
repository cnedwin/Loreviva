package delivery

import (
	"context"
	"loreviva.local/loreviva/internal/generated"
)

// Notification data is safe metadata, but still requires the actual registered
// SERVICE and the original current source/use proof on each owning transaction.
func (s *Service) GetNotifications(ctx context.Context, world, notice, cursor string, limit uint16) (generated.IntegrationNotificationPage, error) {
	ctx, done, err := s.enter(ctx)
	if err != nil {
		return generated.IntegrationNotificationPage{}, err
	}
	defer done()
	return retry(func() (generated.IntegrationNotificationPage, error) {
		out, err := s.store.GetIntegrationNotifications(ctx, s, world, notice, cursor, limit)
		if err == nil {
			_, err = checked("IntegrationNotificationPage", out)
		}
		if err != nil {
			return generated.IntegrationNotificationPage{}, err
		}
		return out, nil
	})
}

func (s *Service) RecordReceipt(ctx context.Context, notice, key string, request generated.IntegrationReceiptRequest) (generated.IntegrationReceipt, error) {
	ctx, done, e := s.enter(ctx)
	if e != nil {
		return generated.IntegrationReceipt{}, e
	}
	defer done()
	// A failed COMMIT is UNKNOWN. Never automatically retry a write here.
	out, e := s.store.RecordIntegrationNotificationReceipt(ctx, s, notice, key, request)
	if e == nil {
		_, e = checked("IntegrationReceipt", out)
	}
	if e != nil {
		return generated.IntegrationReceipt{}, e
	}
	return out, nil
}

// The actual API parent survives the bounded transport write; SQL transactions
// and rows have already ended. No released context is borrowed by another call.
func (s *Service) WriteNotifications(ctx context.Context, world, notice, cursor string, limit uint16, write func([]byte) error) error {
	ctx, done, e := s.enter(ctx)
	if e != nil {
		return e
	}
	defer done()
	out, e := retry(func() (generated.IntegrationNotificationPage, error) {
		return s.store.GetIntegrationNotifications(ctx, s, world, notice, cursor, limit)
	})
	if e != nil {
		return e
	}
	raw, e := checked("IntegrationNotificationPage", out)
	if e != nil {
		return e
	}
	return boundedWrite(ctx, write, raw)
}
func (s *Service) WriteReceipt(ctx context.Context, notice, key string, request generated.IntegrationReceiptRequest, write func([]byte) error) error {
	ctx, done, e := s.enter(ctx)
	if e != nil {
		return e
	}
	defer done()
	out, e := s.store.RecordIntegrationNotificationReceipt(ctx, s, notice, key, request)
	if e != nil {
		return e
	}
	raw, e := checked("IntegrationReceipt", out)
	if e != nil {
		return e
	}
	return boundedWrite(ctx, write, raw)
}
