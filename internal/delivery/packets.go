package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/generated"
	store "loreviva.local/loreviva/internal/store/mysql"
	"strconv"
	"strings"
)

// Only the closed MediaRef fields cross into public_v1. This projection is
// data-only and runs after actual current/file authorization, not instead of it.
func publicMediaProjection(raw []byte) ([]byte, error) {
	var body map[string]any
	if e := json.Unmarshal(raw, &body); e != nil {
		return nil, e
	}
	packet := func(v any) error {
		p, ok := v.(map[string]any)
		if !ok {
			return store.ErrDenied
		}
		items, ok := p["media"].([]any)
		if !ok {
			return store.ErrDenied
		}
		for i, item := range items {
			m, ok := item.(map[string]any)
			if !ok {
				return store.ErrDenied
			}
			n := map[string]any{}
			for _, key := range []string{"media_id", "media_kind", "media_state", "resource_revision", "asset_id"} {
				if x, exists := m[key]; exists {
					n[key] = x
				}
			}
			items[i] = n
		}
		return nil
	}
	if values, ok := body["approved_packets"].([]any); ok {
		for _, v := range values {
			if e := packet(v); e != nil {
				return nil, e
			}
		}
	}
	if values, ok := body["deliveries"].([]any); ok {
		for _, v := range values {
			d, ok := v.(map[string]any)
			if !ok {
				return nil, store.ErrDenied
			}
			if p, exists := d["available_packet"]; exists {
				if e := packet(p); e != nil {
					return nil, e
				}
			}
		}
	}
	return json.Marshal(body)
}

func integrationPacket(p generated.ApprovedPacket) generated.IntegrationApprovedPacket {
	return generated.IntegrationApprovedPacket{PacketID: p.PacketID, TurnID: p.TurnID, Scope: p.Scope, Versions: p.Versions, Audience: p.Audience, TextSegments: p.TextSegments, Actions: p.Actions, Media: []generated.RuntimeMediaRef{}}
}
func packetDelivery(ref store.DeliveryPacketReference, p generated.IntegrationApprovedPacket) (json.RawMessage, error) {
	if !ref.Available {
		return checked("IntegrationDeliveryUpdate", map[string]any{"delivery_id": ref.DeliveryID, "packet_id": ref.PacketID, "packet_revision": strconv.FormatUint(ref.Revision, 10), "delivery_state": "WITHDRAWN", "media_state": ref.MediaState})
	}
	if p.PacketID != ref.PacketID || p.TurnID != ref.TaskID {
		return nil, store.ErrDenied
	}
	return checked("IntegrationDeliveryUpdate", map[string]any{"delivery_id": ref.DeliveryID, "packet_id": ref.PacketID, "packet_revision": strconv.FormatUint(ref.Revision, 10), "delivery_state": "AVAILABLE", "media_state": ref.MediaState, "available_packet": p})
}

var errMediaUnavailable = errors.New("current media file cannot be proven")

func (s *Service) mediaPacket(ctx context.Context, ref store.DeliveryPacketReference, p generated.ApprovedPacket) (generated.IntegrationApprovedPacket, error) {
	out := integrationPacket(p)
	if ref.Kind != "MEDIA" {
		return out, nil
	}
	if !ref.Available {
		return generated.IntegrationApprovedPacket{}, store.ErrDenied
	}
	v := map[string]any{"media_id": ref.MediaID, "media_kind": "AUDIO", "media_state": ref.MediaState, "resource_revision": strconv.FormatUint(ref.Revision, 10)}
	if ref.MediaState == "READY" {
		// GetTurn/EventPage already owns a live delivery parent. This closed
		// descriptor stage uses that parent, never re-enters with a borrowed
		// permit or exports the Store read as a transport capability.
		r, e := s.store.OpenRuntimeMediaRead(ctx, s, ref.AssetID)
		if e != nil {
			return generated.IntegrationApprovedPacket{}, e
		}
		defer r.Close()
		plan, e := r.FilePlan(s.files)
		if e != nil {
			return generated.IntegrationApprovedPacket{}, e
		}
		_, fileErr := s.files.ReadRuntimeMediaBytes(ctx, r, 0, 44)
		if e = r.Finish(ctx); e != nil {
			return generated.IntegrationApprovedPacket{}, e
		}
		// File unavailability is not loss of historical COMMIT or proof that a
		// state transition happened. Only the current wire asset is withheld.
		if fileErr != nil {
			if ctx.Err() != nil {
				return generated.IntegrationApprovedPacket{}, ctx.Err()
			}
			return generated.IntegrationApprovedPacket{}, errMediaUnavailable
		}
		if plan.Revision != ref.Revision || plan.Media != ref.MediaID || plan.Asset != ref.AssetID {
			return generated.IntegrationApprovedPacket{}, store.ErrReplan
		}
		v["asset_id"] = plan.Asset
		v["mime_type"] = plan.MIME
		v["byte_length"] = strconv.FormatUint(plan.Bytes, 10)
		v["content_sha256"] = strings.ToLower(plan.SHA256)
		v["content_path"] = "/world/v1/runtime-media/" + plan.Asset + "/content"
	}
	raw, e := checked("RuntimeMediaRef", v)
	if e != nil {
		return generated.IntegrationApprovedPacket{}, e
	}
	out.Media = append(out.Media, raw)
	return out, nil
}
func (s *Service) turn(ctx context.Context, id string) (generated.IntegrationTurnView, error) {
	return retry(func() (generated.IntegrationTurnView, error) {
		scope, e := s.store.IntegrationTurnScope(ctx, id)
		if e != nil {
			return generated.IntegrationTurnView{}, e
		}
		// Preserve noncommitted metadata's original narrow path, without body-use.
		metadata, e := s.store.ReadTurn(ctx, scope, id)
		if e != nil {
			return generated.IntegrationTurnView{}, e
		}
		out := generated.IntegrationTurnView{TurnID: metadata.TurnID, Task: metadata.Task, Versions: metadata.Versions, Attempts: metadata.Attempts, Billing: metadata.Billing, ApprovedPackets: []generated.IntegrationApprovedPacket{}, Deliveries: []generated.IntegrationDeliveryUpdate{}}
		if metadata.Task.TaskState != "COMMITTED" {
			_, e = checked("IntegrationTurnView", out)
			return out, e
		}
		before, e := s.store.BeginIntegrationPacket(ctx, scope.WorldInstanceID, id)
		if e != nil {
			return generated.IntegrationTurnView{}, e
		}
		refs, e := before.PacketReferences()
		if e != nil || len(refs) != 1 {
			before.Close()
			return generated.IntegrationTurnView{}, store.ErrDenied
		}
		if e = before.Finish(ctx); e != nil {
			return generated.IntegrationTurnView{}, e
		}
		// Original Files reader owns its two current approved-result checks and hash.
		view, e := s.turns.GetTurn(ctx, scope, id)
		if e != nil {
			return generated.IntegrationTurnView{}, e
		}
		projected := []generated.IntegrationApprovedPacket{}
		for _, p := range view.ApprovedPackets {
			v, e := s.mediaPacket(ctx, refs[0], p)
			if errors.Is(e, errMediaUnavailable) {
				refs[0].Available = false
				break
			}
			if e != nil {
				return generated.IntegrationTurnView{}, e
			}
			projected = append(projected, v)
		}
		after, e := s.store.BeginIntegrationPacket(ctx, scope.WorldInstanceID, id)
		if e != nil {
			return generated.IntegrationTurnView{}, e
		}
		defer after.Close()
		if e = after.MatchFinished(before); e != nil {
			return generated.IntegrationTurnView{}, e
		}
		if e = after.Finish(ctx); e != nil {
			return generated.IntegrationTurnView{}, e
		}
		out.Task = view.Task
		out.Versions = view.Versions
		out.Attempts = view.Attempts
		out.Billing = view.Billing
		if !refs[0].Available {
			d, e := packetDelivery(refs[0], generated.IntegrationApprovedPacket{})
			if e != nil {
				return generated.IntegrationTurnView{}, e
			}
			out.Deliveries = append(out.Deliveries, d)
		}
		for _, p := range projected {
			out.ApprovedPackets = append(out.ApprovedPackets, p)
			d, e := packetDelivery(refs[0], p)
			if e != nil {
				return generated.IntegrationTurnView{}, e
			}
			out.Deliveries = append(out.Deliveries, d)
		}
		_, e = checked("IntegrationTurnView", out)
		return out, e
	})
}
func (s *Service) GetTurn(ctx context.Context, id string) (generated.IntegrationTurnView, error) {
	ctx, done, e := s.enter(ctx)
	if e != nil {
		return generated.IntegrationTurnView{}, e
	}
	defer done()
	return s.turn(ctx, id)
}
func (s *Service) WriteTurn(ctx context.Context, id, profile string, write func([]byte) error) error {
	ctx, done, e := s.enter(ctx)
	if e != nil {
		return e
	}
	defer done()
	v, e := s.turn(ctx, id)
	if e != nil {
		return e
	}
	raw, e := checked("IntegrationTurnView", v)
	if e != nil {
		return e
	}
	if profile == "public_v1" {
		raw, e = publicMediaProjection(raw)
		if e != nil {
			return e
		}
		if e = generated.Validate("TurnView", raw); e != nil {
			return e
		}
	} else if profile != "integration_r0_v1" {
		return store.ErrDenied
	}
	return boundedWrite(ctx, write, raw)
}
