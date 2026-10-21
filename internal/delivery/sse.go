package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/generated"
	store "loreviva.local/loreviva/internal/store/mysql"
	"time"
)

func updateFrame(v generated.IntegrationEventPage) ([]byte, error) {
	if _, e := checked("IntegrationSSEUpdate", map[string]any{"event": "update", "id": v.NextCursor, "data": v}); e != nil {
		return nil, e
	}
	data, e := json.Marshal(v)
	if e != nil {
		return nil, e
	}
	return []byte("event: update\nid: " + v.NextCursor + "\ndata: " + string(data) + "\n\n"), nil
}
func resetFrame() []byte {
	v := map[string]any{"code": "VIEW_RESET", "message": "Fetch a new authorized snapshot.", "correlation_id": "local-delivery", "retryable": false, "recovery_action": "FETCH_NEW_SNAPSHOT"}
	if _, e := checked("IntegrationSSEUpdate", map[string]any{"event": "view_reset", "data": v}); e != nil {
		return nil
	}
	b, _ := json.Marshal(v)
	return []byte("event: view_reset\ndata: " + string(b) + "\n\n")
}

// Every iteration enters from the original base context, repeats the sealed
// CLIENT/current gates, and returns the actual parent permit before waiting.
// The HTTP adapter enforces the write deadline on the real connection.
func (s *Service) StreamEvents(base context.Context, world, cursor string, write func([]byte) error) error {
	ctx, cancel := context.WithTimeout(base, 60*time.Second)
	defer cancel()
	heartbeat := time.Now()
	drained := 0
	outputStarted := false
	for {
		if e := ctx.Err(); e != nil {
			return e
		}
		next, done, e := s.enter(ctx)
		if e != nil {
			_ = boundedWrite(ctx, write, resetFrame())
			return e
		}
		page, e := s.events(next, world, cursor, 50)
		advanced := e == nil && page.NextCursor != cursor
		if e == nil {
			if page.NextCursor != cursor {
				var frame []byte
				frame, e = updateFrame(page)
				if e == nil {
					outputStarted = true
					e = boundedWrite(next, write, frame)
				}
				cursor = page.NextCursor
				drained++
			} else if time.Since(heartbeat) >= 15*time.Second {
				outputStarted = true
				e = boundedWrite(next, write, []byte(": heartbeat\n\n"))
				heartbeat = time.Now()
			}
		}
		done()
		if e != nil {
			// Before the first write the adapter can still send the required 409.
			// After output begins, preserve the existing reset-frame/close behavior.
			if !outputStarted && errors.Is(e, store.ErrDeliveryViewReset) {
				return e
			}
			_ = boundedWrite(ctx, write, resetFrame())
			return e
		}
		// EOF is exact cursor equality, not an empty visible result. Empty filtered
		// pages may advance and are drained under the same twenty-page bound.
		if advanced && drained < 20 {
			continue
		}
		drained = 0
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}
