package app

import (
	"errors"
	"io"
	"loreviva.local/loreviva/internal/delivery"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	store "loreviva.local/loreviva/internal/store/mysql"
	"loreviva.local/loreviva/internal/wire"
	"mime"
	"net/http"
	"strconv"
	"time"
)

const snapshotPattern = "GET /world/v1/instances/{instance_id}/snapshot"
const eventsPattern = "GET /world/v1/instances/{instance_id}/events"
const streamPattern = "GET /world/v1/instances/{instance_id}/events/stream"
const turnPattern = "GET /world/v1/turns/{task_id}"
const mediaPattern = "GET /world/v1/runtime-media/{asset_id}/content"
const notificationPattern = "GET /world/v1/customer-notifications"
const receiptPattern = "POST /world/v1/customer-notifications/{notification_ref}/receipts"

// Only the exact delivery registrations own bounded admission. HEAD and unknown
// paths retain the outer guard; notification POST is its own short write stage.
func deliveryPattern(q *http.Request, m *http.ServeMux) bool {
	_, p := m.Handler(q)
	if q.Method == http.MethodPost {
		return p == receiptPattern
	}
	if q.Method != http.MethodGet {
		return false
	}
	return p == snapshotPattern || p == eventsPattern || p == streamPattern || p == turnPattern || p == mediaPattern || p == notificationPattern
}
func (r *Runtime) integrationDeliveryRoutes(m *http.ServeMux) {
	m.HandleFunc(snapshotPattern, r.integrationDeliveryHTTP)
	m.HandleFunc(eventsPattern, r.integrationDeliveryHTTP)
	m.HandleFunc(streamPattern, r.integrationDeliveryHTTP)
	m.HandleFunc(mediaPattern, r.integrationDeliveryHTTP)
	m.HandleFunc(notificationPattern, r.integrationNotificationHTTP)
	m.HandleFunc(receiptPattern, r.integrationNotificationHTTP)
}

func (r *Runtime) integrationNotificationHTTP(w http.ResponseWriter, q *http.Request) {
	if r.Delivery == nil || r.deliveryBindError != nil {
		failure(w, errors.New("delivery unavailable"))
		return
	}
	ctx, _, e := r.Store.AuthenticateIntegration(q.Context(), q.Header.Get("Authorization"), domain.IntegrationService)
	if e != nil {
		response(w, 401, map[string]any{"code": "UNAUTHENTICATED", "message": "Current Service authentication required.", "correlation_id": "local-notification", "retryable": false, "recovery_action": "AUTHENTICATE"})
		return
	}
	if _, e = integrationProfile(q); e != nil {
		failure(w, e)
		return
	}
	values := q.URL.Query()
	world, notice, cursor := "", "", ""
	limit := uint16(20)
	var input generated.IntegrationReceiptRequest
	if q.Pattern == notificationPattern {
		for k, v := range values {
			if len(v) != 1 || (k != "world_instance_id" && k != "notification_ref" && k != "cursor" && k != "limit") {
				failure(w, wire.ErrJSON)
				return
			}
		}
		world, notice, cursor = values.Get("world_instance_id"), values.Get("notification_ref"), values.Get("cursor")
		if domain.ValidateID(world) != nil || notice != "" && domain.ValidateID(notice) != nil || notice != "" && cursor != "" || len(cursor) > 4096 {
			failure(w, wire.ErrJSON)
			return
		}
		if raw := values.Get("limit"); raw != "" {
			v, err := strconv.ParseUint(raw, 10, 16)
			if err != nil || v < 1 || v > 100 || strconv.FormatUint(v, 10) != raw {
				failure(w, wire.ErrJSON)
				return
			}
			limit = uint16(v)
		}
	} else {
		media, _, err := mime.ParseMediaType(q.Header.Get("Content-Type"))
		if err != nil || media != "application/json" || len(values) != 0 || len(q.Header.Values("Idempotency-Key")) != 1 || domain.ValidateIdempotencyKey(q.Header.Get("Idempotency-Key")) != nil {
			failure(w, wire.ErrJSON)
			return
		}
		q.Body = http.MaxBytesReader(w, q.Body, wire.MaxBytes)
		raw, err := io.ReadAll(q.Body)
		if err != nil || generated.Decode("IntegrationReceiptRequest", raw, &input) != nil {
			failure(w, wire.ErrJSON)
			return
		}
	}
	sent := false
	rc := http.NewResponseController(w)
	write := func(raw []byte) error {
		if e := rc.SetWriteDeadline(time.Now().Add(5 * time.Second)); e != nil {
			return e
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		status := http.StatusOK
		if q.Pattern == receiptPattern {
			status = http.StatusCreated
		}
		w.WriteHeader(status)
		sent = true
		if _, e := w.Write(raw); e != nil {
			return e
		}
		if e := rc.Flush(); e != nil {
			return e
		}
		return rc.SetWriteDeadline(time.Time{})
	}
	if q.Pattern == notificationPattern {
		e = r.Delivery.WriteNotifications(ctx, world, notice, cursor, limit, write)
	} else {
		e = r.Delivery.WriteReceipt(ctx, q.PathValue("notification_ref"), q.Header.Get("Idempotency-Key"), input, write)
	}
	if e != nil && !sent {
		failure(w, e)
	}
}
func (r *Runtime) integrationDeliveryHTTP(w http.ResponseWriter, q *http.Request) {
	if q.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if r.Delivery == nil || r.deliveryBindError != nil {
		failure(w, errors.New("delivery unavailable"))
		return
	}
	// Authentication returns a principal context, not a released stage permit.
	ctx, ok := r.integrationClient(w, q)
	if !ok {
		return
	}
	profile, e := integrationProfile(q)
	if e != nil {
		failure(w, e)
		return
	}
	if q.Pattern == mediaPattern {
		if len(q.URL.Query()) != 0 || len(q.Header.Values("Range")) > 1 {
			failure(w, wire.ErrJSON)
			return
		}
		media, e := r.Delivery.ReadRuntimeMedia(ctx, q.PathValue("asset_id"), delivery.MediaRange{Spec: q.Header.Get("Range")})
		if e != nil {
			if errors.Is(e, delivery.ErrMediaRange) {
				w.Header().Set("Content-Range", "bytes */160044")
				response(w, http.StatusRequestedRangeNotSatisfiable, map[string]any{"code": "INVALID_REQUEST", "message": "Requested byte range is not satisfiable.", "correlation_id": "local-media", "retryable": false, "recovery_action": "FIX_REQUEST"})
			} else {
				failure(w, e)
			}
			return
		}
		defer media.Close()
		status, contentRange, length, e := media.ResponseRange()
		if e != nil {
			failure(w, e)
			return
		}
		sent := false
		rc := http.NewResponseController(w)
		e = media.WriteChunks(ctx, func(raw []byte) error {
			if len(raw) > 16384 {
				return wire.ErrJSON
			}
			if e := rc.SetWriteDeadline(time.Now().Add(5 * time.Second)); e != nil {
				return e
			}
			if !sent {
				w.Header().Set("Content-Type", "audio/wav")
				w.Header().Set("Accept-Ranges", "bytes")
				w.Header().Set("Cache-Control", "no-store")
				w.Header().Set("X-Content-Type-Options", "nosniff")
				w.Header().Set("Content-Length", strconv.FormatUint(length, 10))
				if contentRange != "" {
					w.Header().Set("Content-Range", contentRange)
				}
				w.WriteHeader(status)
				sent = true
			}
			if _, e := w.Write(raw); e != nil {
				return e
			}
			if e := rc.Flush(); e != nil {
				return e
			}
			return rc.SetWriteDeadline(time.Time{})
		})
		if e != nil && !sent {
			failure(w, e)
		}
		return
	}
	values := q.URL.Query()
	cursor := ""
	limit := uint16(50)
	pattern := q.Pattern
	if pattern == eventsPattern || pattern == streamPattern {
		for k, v := range values {
			if len(v) != 1 || (k != "cursor" && (k != "limit" || pattern == streamPattern)) {
				failure(w, wire.ErrJSON)
				return
			}
		}
		cursor = values.Get("cursor")
		if len(cursor) > 4096 {
			failure(w, wire.ErrJSON)
			return
		}
		limit, e = delivery.ParseLimit(values.Get("limit"))
		if e != nil {
			failure(w, e)
			return
		}
	} else if len(values) != 0 {
		failure(w, wire.ErrJSON)
		return
	}
	sent := false
	rc := http.NewResponseController(w)
	write := func(raw []byte) error {
		// SQL/rows/read locks have closed before Service invokes this callback.
		// Set a deadline on the actual transport, not merely a timer around Write.
		if e := rc.SetWriteDeadline(time.Now().Add(5 * time.Second)); e != nil {
			return e
		}
		if !sent {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			if pattern == streamPattern {
				w.Header().Set("Content-Type", "text/event-stream")
			} else {
				w.Header().Set("Content-Type", "application/json")
			}
			w.WriteHeader(http.StatusOK)
			sent = true
		}
		if _, e := w.Write(raw); e != nil {
			return e
		}
		if e := rc.Flush(); e != nil {
			return e
		}
		return rc.SetWriteDeadline(time.Time{})
	}
	switch pattern {
	case snapshotPattern:
		e = r.Delivery.WriteSnapshot(ctx, q.PathValue("instance_id"), write)
	case eventsPattern:
		e = r.Delivery.WriteEvents(ctx, q.PathValue("instance_id"), cursor, profile, limit, write)
	case streamPattern:
		e = r.Delivery.StreamEvents(ctx, q.PathValue("instance_id"), cursor, write)
	case turnPattern:
		e = r.Delivery.WriteTurn(ctx, q.PathValue("task_id"), profile, write)
	default:
		e = wire.ErrJSON
	}
	if e != nil && !sent {
		// Only this four-route adapter maps the typed, current-authorized legacy
		// view to a reset. Other authorization failures keep the existing mapping.
		if errors.Is(e, store.ErrDeliveryViewReset) {
			response(w, http.StatusConflict, map[string]any{"code": "VIEW_RESET", "message": "Fetch a new authorized snapshot.", "correlation_id": "local-delivery", "retryable": false, "recovery_action": "FETCH_NEW_SNAPSHOT"})
			return
		}
		failure(w, e)
	}
}
