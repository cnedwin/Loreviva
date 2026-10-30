package content

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"loreviva.local/loreviva/internal/documents"
	"loreviva.local/loreviva/internal/generated"
)

var ErrDocumentMapping = errors.New("document mapping snapshot is incompatible or stale")

// DocumentMappingSelection is an internal program input, not a public protocol
// or an authorization claim. The later owning port must acquire current source
// custody and target authority independently before calling this pure kernel.
type DocumentMappingSelection struct {
	DraftID       generated.Identity
	Revision      generated.ExactUnsigned
	ContentHash   string
	ObjectID      generated.Identity
	ObjectHash    string
	SourceAssetID generated.Identity
	SourceSHA256  string
	ParserVersion string
	ChunkIndex    int
	ChunkSHA256   string
}

// DocumentMappingProposal stores only an uncommitted deterministic difference.
// There is deliberately no API, database writer or generic-save integration.
// A matching digest proves snapshot consistency, not authenticity or permission.
type DocumentMappingProposal struct {
	selection  DocumentMappingSelection
	candidate  generated.DraftInput
	provenance []byte
	digest     string
}

func mappingClone(in generated.DraftInput) (generated.DraftInput, error) {
	var out generated.DraftInput
	b, e := json.Marshal(in)
	if e != nil {
		return out, ErrDocumentMapping
	}
	if json.Unmarshal(b, &out) != nil {
		return out, ErrDocumentMapping
	}
	// Preserve untouched object bytes as well as isolating every nested pointer.
	for i, raw := range in.Objects {
		out.Objects[i] = append(json.RawMessage(nil), raw...)
	}
	return out, nil
}

func (p DocumentMappingProposal) Digest() string { return p.digest }

// CopyCandidate is for the future protected owning transaction. Passing these
// optional source fields to ordinary Save does NOT establish protected lineage.
func (p DocumentMappingProposal) CopyCandidate() (generated.DraftInput, error) {
	if p.digest == "" {
		return generated.DraftInput{}, ErrDocumentMapping
	}
	return mappingClone(p.candidate)
}
func (p DocumentMappingProposal) CopyProvenance() []byte { return append([]byte(nil), p.provenance...) }

func mappingSnapshot(v any, schema string) ([]byte, string, error) {
	if !mappingShape(v) {
		return nil, "", ErrDocumentMapping
	}
	b, e := json.Marshal(v)
	if e != nil || len(b) > 1<<20 {
		return nil, "", ErrDocumentMapping
	}
	if schema != "" && generated.Validate(schema, b) != nil {
		return nil, "", ErrDocumentMapping
	}
	h, e := Hash(json.RawMessage(b))
	if e != nil {
		return nil, "", ErrDocumentMapping
	}
	return b, h, nil
}

// Go's JSON encoder replaces malformed UTF8; refusing it here prevents an
// in-memory malformed snapshot from acquiring a digest for different text.
// Inspect the complete shape before JSON allocation, including metadata and
// selection fields. The size estimate accounts for Go's JSON string/HTML escapes;
// each snapshot has its own byte budget and a bounded traversal node count.
func mappingShape(value any) bool {
	bytesLeft, nodesLeft := int64(1<<20), 100000
	take := func(n int64) bool {
		if n < 0 || n > bytesLeft {
			return false
		}
		bytesLeft -= n
		return true
	}
	var visit func(reflect.Value, int) bool
	stringSize := func(s string) bool {
		if int64(len(s))+2 > bytesLeft || !utf8.ValidString(s) || !take(2) {
			return false
		}
		for _, r := range s {
			n := utf8.RuneLen(r)
			switch r {
			case '"', '\\', '\b', '\f', '\n', '\r', '\t':
				n = 2
			case '<', '>', '&', 0x2028, 0x2029:
				n = 6
			default:
				if r < 0x20 {
					n = 6
				}
			}
			if !take(int64(n)) {
				return false
			}
		}
		return true
	}
	visit = func(v reflect.Value, depth int) bool {
		nodesLeft--
		if depth > 32 || nodesLeft < 0 {
			return false
		}
		if !v.IsValid() {
			return take(4)
		}
		switch v.Kind() {
		case reflect.String:
			return stringSize(v.String())
		case reflect.Pointer, reflect.Interface:
			if v.IsNil() {
				return take(4)
			}
			return visit(v.Elem(), depth+1)
		case reflect.Struct:
			if !take(2) {
				return false
			}
			typ := v.Type()
			for i := 0; i < v.NumField(); i++ {
				field := typ.Field(i)
				name := strings.Split(field.Tag.Get("json"), ",")[0]
				if name == "-" {
					continue
				}
				if name == "" {
					name = field.Name
				}
				if !stringSize(name) || !take(2) || !visit(v.Field(i), depth+1) {
					return false
				}
			}
			return true
		case reflect.Slice, reflect.Array:
			if v.Kind() == reflect.Slice && v.IsNil() {
				return take(4)
			}
			if v.Kind() == reflect.Slice && v.Type().Elem().Kind() == reflect.Uint8 {
				if int64(v.Len()) > bytesLeft {
					return false
				}
				b := v.Bytes()
				if !utf8.Valid(b) || !take(int64(len(b))) {
					return false
				}
				// RawMessage is copied/compacted by encoding/json, with HTML escapes.
				for _, r := range string(b) {
					switch r {
					case '<', '>', '&':
						if !take(5) {
							return false
						}
					case 0x2028, 0x2029:
						if !take(3) {
							return false
						}
					}
				}
				return true
			}
			if v.Len() > nodesLeft || !take(2) {
				return false
			}
			for i := 0; i < v.Len(); i++ {
				if !take(1) || !visit(v.Index(i), depth+1) {
					return false
				}
			}
			return true
		case reflect.Map:
			if v.IsNil() {
				return take(4)
			}
			if v.Len() > nodesLeft/2 || !take(2) {
				return false
			}
			it := v.MapRange()
			for it.Next() {
				if !take(2) || !visit(it.Key(), depth+1) || !visit(it.Value(), depth+1) {
					return false
				}
			}
			return true
		case reflect.Bool:
			return take(5)
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Float32, reflect.Float64:
			return take(24)
		default:
			return false
		}
	}
	return visit(reflect.ValueOf(value), 0)
}

// ProposeDocumentMapping narrows a single literal COMPLETE chunk into an empty
// NOTE. It never invokes models, mutates a draft, saves, publishes or mints a
// persistent extraction-report ID. Partial/merged knowledge needs a later port.
func ProposeDocumentMapping(d generated.ContentDraft, source generated.DocumentPreview, sel DocumentMappingSelection, at time.Time) (DocumentMappingProposal, error) {
	zero := DocumentMappingProposal{}
	if at.IsZero() || d.DraftRevision == "0" || d.State != "DRAFT" || d.DraftID != sel.DraftID || d.DraftRevision != sel.Revision || d.ContentHash != sel.ContentHash || d.Scope != source.Asset.Scope {
		return zero, ErrDocumentMapping
	}
	// Bound existing raw objects/text before marshaling combined snapshots.
	n := 0
	for _, raw := range d.Input.Objects {
		n += len(raw)
		if n > 1<<20 {
			return zero, ErrDocumentMapping
		}
	}
	if len(source.Report.Chunks) > documents.MaxChunks {
		return zero, ErrDocumentMapping
	}
	n = 0
	for _, c := range source.Report.Chunks {
		n += len(c.Text)
		if n > documents.MaxOutputBytes {
			return zero, ErrDocumentMapping
		}
	}
	_, targetSnapshot, e := mappingSnapshot(d, "ContentDraft")
	if e != nil {
		return zero, e
	}
	_, sourceSnapshot, e := mappingSnapshot(source.Asset, "FileRecord")
	if e != nil {
		return zero, e
	}
	_, reportSnapshot, e := mappingSnapshot(source.Report, "DocumentExtractionReport")
	if e != nil {
		return zero, e
	}
	hash, e := Hash(d.Input)
	if e != nil || hash != d.ContentHash {
		return zero, ErrDocumentMapping
	}
	if _, e = objects(d.Input); e != nil {
		return zero, ErrDocumentMapping
	}
	a, r := source.Asset, source.Report
	if a.State != "READY" || a.Purpose != "KNOWLEDGE" || a.AssetID != sel.SourceAssetID || a.Sha256 != sel.SourceSHA256 || r.SourceSha256 != a.Sha256 || r.SizeBytes != a.SizeBytes || r.ParserVersion != documents.ParserVersion || sel.ParserVersion != r.ParserVersion || r.Status != "COMPLETE" || len(r.Missing) != 0 || r.Ocr || r.Adoptable || r.ExecutionReady {
		return zero, ErrDocumentMapping
	}
	formats := map[string]string{"text/plain": "TXT", "text/markdown": "MD", "application/pdf": "PDF", "application/vnd.openxmlformats-officedocument.wordprocessingml.document": "DOCX"}
	if formats[a.MediaType] == "" || formats[a.MediaType] != r.Format {
		return zero, ErrDocumentMapping
	}
	size, e := strconv.ParseUint(string(a.SizeBytes), 10, 64)
	if e != nil || size == 0 || size > documents.MaxInputBytes {
		return zero, ErrDocumentMapping
	}
	uses := map[string]bool{}
	for _, u := range a.Rights.AllowedUses {
		uses[u] = true
	}
	if !uses["EDIT"] || !uses["COPY"] || strings.TrimSpace(a.Rights.License) == "" {
		return zero, ErrDocumentMapping
	}
	if a.Rights.ExpiresAt != nil {
		expiry, e := time.Parse(time.RFC3339Nano, *a.Rights.ExpiresAt)
		if e != nil || !at.Before(expiry) {
			return zero, ErrDocumentMapping
		}
	}
	if sel.ChunkIndex < 0 || sel.ChunkIndex >= len(r.Chunks) {
		return zero, ErrDocumentMapping
	}
	c := r.Chunks[sel.ChunkIndex]
	sum := sha256.Sum256([]byte(c.Text))
	if c.Text == "" || utf8.RuneCountInString(c.Text) > 10000 || c.Sha256 != sel.ChunkSHA256 || hex.EncodeToString(sum[:]) != c.Sha256 {
		return zero, ErrDocumentMapping
	}
	var note generated.KnowledgeDefinition
	index := -1
	for i, raw := range d.Input.Objects {
		var header struct {
			ObjectID generated.Identity `json:"object_id"`
		}
		if json.Unmarshal(raw, &header) != nil {
			return zero, ErrDocumentMapping
		}
		if header.ObjectID == sel.ObjectID {
			if index != -1 || generated.Decode("KnowledgeDefinition", raw, &note) != nil {
				return zero, ErrDocumentMapping
			}
			index = i
			h, e := Hash(raw)
			if e != nil || h != sel.ObjectHash {
				return zero, ErrDocumentMapping
			}
		}
	}
	if index < 0 || note.Kind != "KNOWLEDGE" || note.KnowledgeType != "NOTE" || note.Text != "" || note.OriginRef != nil || len(note.SourceRefs) != 0 || len(note.DependencyRefs) != 0 || note.SourceAssetID != nil || note.SourceAssetHash != nil || note.SourceLocation != nil || note.ExtractionReportID != nil {
		return zero, ErrDocumentMapping
	}
	if note.AudiencePolicy != "PRODUCT_EDITORS" && note.AudiencePolicy != "PRODUCT_READERS" {
		return zero, ErrDocumentMapping
	}
	audience := note.AudiencePolicy
	if a.AudiencePolicy == "PRODUCT_EDITORS" {
		audience = "PRODUCT_EDITORS"
	} else if a.AudiencePolicy != "PRODUCT_READERS" {
		return zero, ErrDocumentMapping
	}
	// Clone the source rights before assigning to avoid retaining caller pointers.
	rightsRaw, _, e := mappingSnapshot(a.Rights, "SourceRights")
	if e != nil {
		return zero, e
	}
	var rights generated.SourceRights
	if json.Unmarshal(rightsRaw, &rights) != nil {
		return zero, ErrDocumentMapping
	}
	note.Text = c.Text
	note.Rights = rights
	note.AudiencePolicy = audience
	asset, sha, location := a.AssetID, a.Sha256, c.Location
	note.SourceAssetID = &asset
	note.SourceAssetHash = &sha
	note.SourceLocation = &location
	raw, _, e := mappingSnapshot(note, "KnowledgeDefinition")
	if e != nil {
		return zero, e
	}
	candidate, e := mappingClone(d.Input)
	if e != nil {
		return zero, e
	}
	candidate.Objects[index] = raw
	_, candidateHash, e := mappingSnapshot(candidate, "DraftInput")
	if e != nil {
		return zero, e
	}
	_, selectionHash, e := mappingSnapshot(sel, "")
	if e != nil {
		return zero, e
	}
	// Hash separately bounded snapshots, then a small domain-separated record.
	// Do not concatenate two near-limit documents into a second JSON authority.
	provenance, _, e := mappingSnapshot(struct {
		Format            string                   `json:"format"`
		TargetSnapshot    string                   `json:"target_snapshot_sha256"`
		SourceSnapshot    string                   `json:"source_snapshot_sha256"`
		ReportSnapshot    string                   `json:"report_snapshot_sha256"`
		SelectionSnapshot string                   `json:"selection_snapshot_sha256"`
		CandidateSnapshot string                   `json:"candidate_snapshot_sha256"`
		Asset             generated.FileRecord     `json:"source"`
		Selection         DocumentMappingSelection `json:"selection"`
		Location          string                   `json:"location"`
	}{"loreviva.document-mapping.internal.v1", targetSnapshot, sourceSnapshot, reportSnapshot, selectionHash, candidateHash, a, sel, c.Location}, "")
	if e != nil {
		return zero, e
	}
	digest, e := Hash(json.RawMessage(provenance))
	if e != nil {
		return zero, ErrDocumentMapping
	}
	return DocumentMappingProposal{selection: sel, candidate: candidate, provenance: append([]byte(nil), provenance...), digest: digest}, nil
}

// RecheckDocumentMapping must receive fresh program-owned snapshots at the
// eventual save boundary. It does not query current grants or acquire custody.
func RecheckDocumentMapping(p DocumentMappingProposal, d generated.ContentDraft, source generated.DocumentPreview, at time.Time) (generated.DraftInput, error) {
	if p.digest == "" {
		return generated.DraftInput{}, ErrDocumentMapping
	}
	next, e := ProposeDocumentMapping(d, source, p.selection, at)
	if e != nil || next.digest != p.digest {
		return generated.DraftInput{}, ErrDocumentMapping
	}
	a, e := Hash(p.candidate)
	b, f := Hash(next.candidate)
	provenance, g := Hash(json.RawMessage(p.provenance))
	if e != nil || f != nil || g != nil || a != b || provenance != p.digest {
		return generated.DraftInput{}, ErrDocumentMapping
	}
	return next.CopyCandidate()
}
