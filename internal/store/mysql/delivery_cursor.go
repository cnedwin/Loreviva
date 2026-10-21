package mysql

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"time"
)

const deliveryCursorDomain = "loreviva/delivery-cursor/v1"

// ErrDeliveryViewReset identifies only an authenticated, current-bound legacy
// view. Invalid, expired or foreign cursors remain ordinary authorization denial.
var ErrDeliveryViewReset = errors.New("delivery view reset required")

// This closed, authenticated record is data, never an authorization capability.
// Current CLIENT, source and consent checks run independently for every read.
type deliveryCursorBinding struct {
	Installation, Tenant, Product, Customer, Player, World, Session, Profile  string
	KeyVersion, WorldPrivacy, WorldView, SubjectPrivacy, SubjectView, Consent uint64
}
type deliveryCursorRecord struct {
	Version                              uint64
	Binding                              deliveryCursorBinding
	HighWater, Position                  uint64
	ItemOffset                           uint64
	PartialUpdateID, PartialManifestHash string
	Issued, Expires                      time.Time
}

func (r *IntegrationRead) cursorBinding() deliveryCursorBinding {
	s := r.state
	return deliveryCursorBinding{s.record.Installation, s.record.Tenant, s.record.Product, s.record.Customer, s.record.Player, s.record.World, s.record.ID, s.record.Profile, s.record.KeyVersion, s.versions.PrivacyEpoch, s.versions.ViewEpoch, s.subjectPrivacy, s.subjectView, s.choice.Use.Revision}
}

// CONFIG material is read before opening the SQL transaction. GET never mints
// or repairs a key, and the derived encryption key never leaves this Store.
func (r *IntegrationRead) loadCursorKey() error {
	if r.store.integrationSessionRoot == nil || r.state.record.KeyVersion != 1 {
		return ErrDenied
	}
	material, err := r.store.integrationSessionRoot.SessionHMACKey(r.state.record.Installation, false)
	if err != nil {
		return ErrDenied
	}
	ref := sha256.Sum256(append([]byte(r.state.record.Installation+"/1/"), material[:]...))
	if hex.EncodeToString(ref[:]) != r.state.record.KeyRef {
		return ErrDenied
	}
	h := hmac.New(sha256.New, material[:])
	_, _ = h.Write([]byte(deliveryCursorDomain))
	copy(r.cursorKey[:], h.Sum(nil))
	return nil
}
func deliveryGCM(key [32]byte) (cipher.AEAD, error) {
	c, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(c)
}
func (r *IntegrationRead) sealCursor(v deliveryCursorRecord) (string, error) {
	if r == nil || r.closed || r.tx == nil {
		return "", ErrDenied
	}
	a, err := deliveryGCM(r.cursorKey)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, a.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return "", err
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sealed := a.Seal(nonce, nonce, raw, []byte(deliveryCursorDomain))
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}
func (r *IntegrationRead) openCursor(raw string) (deliveryCursorRecord, error) {
	var v deliveryCursorRecord
	if r == nil || r.closed || len(raw) == 0 || len(raw) > 4096 {
		return v, ErrDenied
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(raw)
	if err != nil {
		return v, ErrDenied
	}
	a, err := deliveryGCM(r.cursorKey)
	if err != nil || len(b) < a.NonceSize()+a.Overhead() {
		return v, ErrDenied
	}
	plain, err := a.Open(nil, b[:a.NonceSize()], b[a.NonceSize():], []byte(deliveryCursorDomain))
	if err != nil {
		return v, ErrDenied
	}
	d := json.NewDecoder(bytes.NewReader(plain))
	d.DisallowUnknownFields()
	if d.Decode(&v) != nil || d.Decode(new(any)) != io.EOF || !reflect.DeepEqual(v.Binding, r.cursorBinding()) || v.Position > v.HighWater || v.HighWater > r.state.h || !v.Issued.Add(15*time.Minute).Equal(v.Expires) || v.Issued.After(r.now) || !r.now.Before(v.Expires) {
		return deliveryCursorRecord{}, ErrDenied
	}
	if v.Version == 1 {
		// Do not reinterpret fmt2 partials or default missing legacy fields.
		fields, ok := deliveryClosedObject(plain, []string{"Version", "Binding", "HighWater", "Position", "Issued", "Expires"})
		if !ok {
			return deliveryCursorRecord{}, ErrDenied
		}
		if _, ok = deliveryClosedObject(fields["Binding"], []string{"Installation", "Tenant", "Product", "Customer", "Player", "World", "Session", "Profile", "KeyVersion", "WorldPrivacy", "WorldView", "SubjectPrivacy", "SubjectView", "Consent"}); !ok {
			return deliveryCursorRecord{}, ErrDenied
		}
		return deliveryCursorRecord{}, ErrDeliveryViewReset
	}
	if v.Version != 2 {
		return deliveryCursorRecord{}, ErrDenied
	}
	// Progress is a closed tuple. A partial is checked against the actual next
	// row and its entire immutable manifest after original proof collection.
	if v.ItemOffset == 0 {
		if v.PartialUpdateID != "" || v.PartialManifestHash != "" {
			return deliveryCursorRecord{}, ErrDenied
		}
	} else {
		h, err := hex.DecodeString(v.PartialManifestHash)
		if v.PartialUpdateID == "" || err != nil || len(h) != 32 || hex.EncodeToString(h) != v.PartialManifestHash || v.Position >= v.HighWater {
			return deliveryCursorRecord{}, ErrDenied
		}
	}
	return v, nil
}

// Legacy recognition requires each original field exactly once, in its exact
// spelling. JSON null, duplicate and unrecognized fields are not legacy data.
func deliveryClosedObject(raw []byte, names []string) (map[string]json.RawMessage, bool) {
	d := json.NewDecoder(bytes.NewReader(raw))
	start, e := d.Token()
	if e != nil || start != json.Delim('{') {
		return nil, false
	}
	allowed := map[string]bool{}
	for _, name := range names {
		allowed[name] = true
	}
	out := map[string]json.RawMessage{}
	for d.More() {
		key, e := d.Token()
		name, ok := key.(string)
		if e != nil || !ok || !allowed[name] || out[name] != nil {
			return nil, false
		}
		var value json.RawMessage
		if d.Decode(&value) != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, false
		}
		out[name] = value
	}
	end, e := d.Token()
	if e != nil || end != json.Delim('}') || d.Decode(new(any)) != io.EOF || len(out) != len(names) {
		return nil, false
	}
	return out, true
}
func (r *IntegrationRead) initialCursor(position uint64) (string, error) {
	return r.sealCursor(deliveryCursorRecord{Version: 2, Binding: r.cursorBinding(), HighWater: r.state.h, Position: position, Issued: r.now, Expires: r.now.Add(15 * time.Minute)})
}
