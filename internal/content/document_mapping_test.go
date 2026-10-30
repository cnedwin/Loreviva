package content

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"loreviva.local/loreviva/internal/documents"
	"loreviva.local/loreviva/internal/generated"
)

func mappingFixture(t *testing.T, text string) (generated.ContentDraft, generated.DocumentPreview, DocumentMappingSelection, time.Time) {
	t.Helper()
	at := time.Date(2026, 10, 30, 4, 0, 0, 0, time.UTC)
	report, e := documents.Extract(context.Background(), []byte(text), "TXT")
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(report)
	var parsed generated.DocumentExtractionReport
	if e = generated.Decode("DocumentExtractionReport", raw, &parsed); e != nil {
		t.Fatal(e)
	}
	rights := generated.SourceRights{AllowedUses: []string{"EDIT", "COPY"}, OriginKind: "LICENSED", SourceLabel: "Synthetic original document", License: "Apache-2.0"}
	scope := generated.ProductScope{TenantID: "demo-local", ProductID: "coffee"}
	asset := generated.FileRecord{AssetID: "source-document", Scope: scope, OwnerSubjectID: "source-owner", State: "READY", Purpose: "KNOWLEDGE", Name: "source.txt", MediaType: "text/plain", Sha256: parsed.SourceSha256, SizeBytes: parsed.SizeBytes, AudiencePolicy: "PRODUCT_EDITORS", Rights: rights, CreatedAt: at.Format(time.RFC3339Nano)}
	note := generated.KnowledgeDefinition{ObjectID: "note", Kind: "KNOWLEDGE", Name: "Empty knowledge", Description: "Keep fields unchanged", Tags: []string{"tag"}, Rights: generated.SourceRights{AllowedUses: []string{"EDIT", "COPY", "PUBLISH", "REDISTRIBUTE"}, OriginKind: "SYNTHETIC", SourceLabel: "Empty original target", License: "CC0-1.0"}, AudiencePolicy: "PRODUCT_READERS", Version: "1", SourceRefs: []string{}, DependencyRefs: []generated.DefinitionRef{}, KnowledgeType: "NOTE", Text: "", BindingRefs: []generated.DefinitionRef{}}
	noteRaw, _ := json.Marshal(note)
	other := append([]byte(nil), noteRaw...)
	other = bytes.Replace(other, []byte(`"object_id":"note"`), []byte(`"object_id":"other"`), 1)
	input := generated.DraftInput{Name: "Document mapping", Description: "Unchanged envelope", Tags: []string{}, RootObjectID: "note", Objects: []generated.ContentObject{noteRaw, other}, ModuleAgentBindings: []generated.ModuleAgentBinding{}, SkillBindings: []generated.SkillBinding{}, DependencyLocks: []generated.DependencyLock{}}
	h, _ := Hash(input)
	objectHash, _ := Hash(json.RawMessage(noteRaw))
	d := generated.ContentDraft{DraftID: "mapping-draft", DraftRevision: "1", ContentHash: h, Input: input, RootKind: "KNOWLEDGE", State: "DRAFT", Scope: scope, AuthorSubjectID: "creator", ModifiedBySubjectID: "creator", CreatedAt: at.Format(time.RFC3339Nano), ModifiedAt: at.Format(time.RFC3339Nano), ValidationSummary: generated.ValidationSummary{BlockingCount: "0", Findings: []generated.FieldError{}}}
	source := generated.DocumentPreview{Asset: asset, Report: parsed}
	sel := DocumentMappingSelection{DraftID: d.DraftID, Revision: d.DraftRevision, ContentHash: h, ObjectID: "note", ObjectHash: objectHash, SourceAssetID: asset.AssetID, SourceSHA256: asset.Sha256, ParserVersion: documents.ParserVersion, ChunkIndex: 0, ChunkSHA256: parsed.Chunks[0].Sha256}
	for name, value := range map[string]any{"ContentDraft": d, "FileRecord": asset, "DocumentExtractionReport": parsed, "KnowledgeDefinition": note} {
		raw, _ := json.Marshal(value)
		if e := generated.Validate(name, raw); e != nil {
			t.Fatalf("synthetic fixture %s invalid: %v", name, e)
		}
	}
	return d, source, sel, at
}

func TestDocumentMappingActualParserClosedProjection(t *testing.T) {
	d, source, sel, at := mappingFixture(t, "A player may decline. <script>never execute</script>")
	before, _ := json.Marshal(d)
	originalSource, _ := json.Marshal(source)
	p, e := ProposeDocumentMapping(d, source, sel, at)
	if e != nil {
		t.Fatal(e)
	}
	candidate, e := p.CopyCandidate()
	if e != nil {
		t.Fatal(e)
	}
	var note generated.KnowledgeDefinition
	if generated.Decode("KnowledgeDefinition", candidate.Objects[0], &note) != nil {
		t.Fatal("candidate")
	}
	if note.Text != source.Report.Chunks[0].Text || note.AudiencePolicy != "PRODUCT_EDITORS" || note.Rights.License != "Apache-2.0" || len(note.Rights.AllowedUses) != 2 || note.SourceAssetID == nil || *note.SourceAssetID != source.Asset.AssetID || note.SourceAssetHash == nil || *note.SourceAssetHash != source.Asset.Sha256 || note.SourceLocation == nil || *note.SourceLocation != source.Report.Chunks[0].Location || note.ExtractionReportID != nil {
		t.Fatal("closed source fields or rights differ", note)
	}
	if note.ObjectID != "note" || note.Name != "Empty knowledge" || note.Description != "Keep fields unchanged" || note.Version != "1" || note.KnowledgeType != "NOTE" || !bytes.Equal(candidate.Objects[1], d.Input.Objects[1]) {
		t.Fatal("unselected field or object changed")
	}
	after, _ := json.Marshal(d)
	afterSource, _ := json.Marshal(source)
	if !bytes.Equal(before, after) || !bytes.Equal(originalSource, afterSource) {
		t.Fatal("caller snapshot mutated")
	}
	q, e := ProposeDocumentMapping(d, source, sel, at.Add(time.Minute))
	if e != nil || q.Digest() != p.Digest() {
		t.Fatal("identical snapshots not deterministic", e)
	}
	if _, e = RecheckDocumentMapping(p, d, source, at.Add(time.Minute)); e != nil {
		t.Fatal("fresh identical snapshots refused", e)
	}
	if source.Report.Adoptable || source.Report.ExecutionReady {
		t.Fatal("preview authority changed")
	}
}

func TestDocumentMappingRefusesStaleOrUnsupportedSnapshots(t *testing.T) {
	cases := map[string]func(*generated.ContentDraft, *generated.DocumentPreview, *DocumentMappingSelection){
		"scope": func(d *generated.ContentDraft, p *generated.DocumentPreview, s *DocumentMappingSelection) {
			p.Asset.Scope.ProductID = "another"
		},
		"target revision": func(d *generated.ContentDraft, p *generated.DocumentPreview, s *DocumentMappingSelection) {
			s.Revision = "2"
		},
		"target content hash": func(d *generated.ContentDraft, p *generated.DocumentPreview, s *DocumentMappingSelection) {
			s.ContentHash = strings.Repeat("a", 64)
		},
		"target object hash": func(d *generated.ContentDraft, p *generated.DocumentPreview, s *DocumentMappingSelection) {
			s.ObjectHash = strings.Repeat("a", 64)
		},
		"missing target": func(d *generated.ContentDraft, p *generated.DocumentPreview, s *DocumentMappingSelection) {
			s.ObjectID = "missing"
		},
		"published target": func(d *generated.ContentDraft, p *generated.DocumentPreview, s *DocumentMappingSelection) {
			d.State = "PUBLISHED"
		},
		"source id": func(d *generated.ContentDraft, p *generated.DocumentPreview, s *DocumentMappingSelection) {
			s.SourceAssetID = "another"
		},
		"source raw hash": func(d *generated.ContentDraft, p *generated.DocumentPreview, s *DocumentMappingSelection) {
			s.SourceSHA256 = strings.Repeat("a", 64)
		},
		"source size": func(d *generated.ContentDraft, p *generated.DocumentPreview, s *DocumentMappingSelection) {
			p.Asset.SizeBytes = "1"
		},
		"source media": func(d *generated.ContentDraft, p *generated.DocumentPreview, s *DocumentMappingSelection) {
			p.Asset.MediaType = "application/pdf"
		},
		"source state": func(d *generated.ContentDraft, p *generated.DocumentPreview, s *DocumentMappingSelection) {
			p.Asset.State = "PURGED"
		},
		"blank licence": func(d *generated.ContentDraft, p *generated.DocumentPreview, s *DocumentMappingSelection) {
			p.Asset.Rights.License = " \t "
		},
		"zero source size": func(d *generated.ContentDraft, p *generated.DocumentPreview, s *DocumentMappingSelection) {
			p.Asset.SizeBytes = "0"
			p.Report.SizeBytes = "0"
		},
		"zero saved revision": func(d *generated.ContentDraft, p *generated.DocumentPreview, s *DocumentMappingSelection) {
			d.DraftRevision = "0"
			s.Revision = "0"
		},
		"no COPY": func(d *generated.ContentDraft, p *generated.DocumentPreview, s *DocumentMappingSelection) {
			p.Asset.Rights.AllowedUses = []string{"EDIT"}
		},
		"no EDIT": func(d *generated.ContentDraft, p *generated.DocumentPreview, s *DocumentMappingSelection) {
			p.Asset.Rights.AllowedUses = []string{"COPY"}
		},
		"partial": func(d *generated.ContentDraft, p *generated.DocumentPreview, s *DocumentMappingSelection) {
			p.Report.Status = "PARTIAL"
		},
		"missing": func(d *generated.ContentDraft, p *generated.DocumentPreview, s *DocumentMappingSelection) {
			p.Report.Missing = []generated.DocumentMissing{{Code: "SOURCE_GAP", Location: "paragraph1"}}
		},
		"OCR": func(d *generated.ContentDraft, p *generated.DocumentPreview, s *DocumentMappingSelection) {
			p.Report.Ocr = true
		},
		"old parser": func(d *generated.ContentDraft, p *generated.DocumentPreview, s *DocumentMappingSelection) {
			s.ParserVersion = "old"
		},
		"negative chunk": func(d *generated.ContentDraft, p *generated.DocumentPreview, s *DocumentMappingSelection) {
			s.ChunkIndex = -1
		},
		"missing chunk": func(d *generated.ContentDraft, p *generated.DocumentPreview, s *DocumentMappingSelection) {
			s.ChunkIndex = 1
		},
		"chunk digest": func(d *generated.ContentDraft, p *generated.DocumentPreview, s *DocumentMappingSelection) {
			p.Report.Chunks[0].Text = "Changed text"
		},
		"invalid UTF8": func(d *generated.ContentDraft, p *generated.DocumentPreview, s *DocumentMappingSelection) {
			p.Asset.OwnerSubjectID = generated.Identity(string([]byte{255}))
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			d, p, s, at := mappingFixture(t, "Exact source")
			change(&d, &p, &s)
			if _, e := ProposeDocumentMapping(d, p, s, at); e == nil {
				t.Fatal("inconsistent snapshot accepted")
			}
		})
	}
}

func TestDocumentMappingExistingProvenanceAndDuplicateIdentityRefused(t *testing.T) {
	cases := map[string]func(map[string]any){"nonempty": func(n map[string]any) { n["text"] = "Existing note" }, "source refs": func(n map[string]any) { n["source_refs"] = []string{"existing source"} }, "source asset": func(n map[string]any) { n["source_asset_id"] = "existing-asset" }, "source hash": func(n map[string]any) { n["source_asset_hash"] = strings.Repeat("a", 64) }, "source location": func(n map[string]any) { n["source_location"] = "page1" }, "report": func(n map[string]any) { n["extraction_report_id"] = "existing-report" }, "extract": func(n map[string]any) { n["knowledge_type"] = "DOCUMENT_EXTRACT" }}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			d, p, s, at := mappingFixture(t, "Source")
			var note map[string]any
			json.Unmarshal(d.Input.Objects[0], &note)
			change(note)
			d.Input.Objects[0] = mustMarshal(note)
			d.ContentHash, _ = Hash(d.Input)
			s.ContentHash = d.ContentHash
			s.ObjectHash, _ = Hash(d.Input.Objects[0])
			if _, e := ProposeDocumentMapping(d, p, s, at); e == nil {
				t.Fatal("existing source overwritten")
			}
		})
	}
	d, p, s, at := mappingFixture(t, "Source")
	d.Input.Objects[1] = append([]byte(nil), d.Input.Objects[0]...)
	d.ContentHash, _ = Hash(d.Input)
	s.ContentHash = d.ContentHash
	if _, e := ProposeDocumentMapping(d, p, s, at); e == nil {
		t.Fatal("duplicate identity accepted")
	}
}

func TestDocumentMappingUnicodeLimitWithoutTruncation(t *testing.T) {
	d, p, s, at := mappingFixture(t, strings.Repeat("界", 10000))
	q, e := ProposeDocumentMapping(d, p, s, at)
	if e != nil {
		t.Fatal("10000 code points refused", e)
	}
	candidate, _ := q.CopyCandidate()
	var note generated.KnowledgeDefinition
	generated.Decode("KnowledgeDefinition", candidate.Objects[0], &note)
	if note.Text != p.Report.Chunks[0].Text {
		t.Fatal("literal text changed")
	}
	p.Report.Chunks[0].Text += "界"
	h := sha256.Sum256([]byte(p.Report.Chunks[0].Text))
	p.Report.Chunks[0].Sha256 = hex.EncodeToString(h[:])
	s.ChunkSHA256 = p.Report.Chunks[0].Sha256
	if _, e = ProposeDocumentMapping(d, p, s, at); e == nil {
		t.Fatal("oversized whole chunk was silently truncated or accepted")
	}
}

func TestDocumentMappingFreshRecheckAndExpiry(t *testing.T) {
	d, p, s, at := mappingFixture(t, "Exact source")
	expiry := at.Add(time.Minute).Format(time.RFC3339Nano)
	p.Asset.Rights.ExpiresAt = &expiry
	q, e := ProposeDocumentMapping(d, p, s, at)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = RecheckDocumentMapping(q, d, p, at.Add(time.Minute)); e == nil {
		t.Fatal("expiry at exact boundary reused")
	}
	for _, change := range []func(*generated.DocumentPreview){func(v *generated.DocumentPreview) { v.Asset.OwnerSubjectID = "changed-owner" }, func(v *generated.DocumentPreview) { v.Asset.Rights.License = "Changed licence" }, func(v *generated.DocumentPreview) { v.Asset.AudiencePolicy = "PRODUCT_READERS" }, func(v *generated.DocumentPreview) { v.Asset.Name = "renamed.txt" }, func(v *generated.DocumentPreview) { v.Asset.Rights.AllowedUses = []string{"EDIT"} }} {
		d, p, s, at := mappingFixture(t, "Exact source")
		q, e := ProposeDocumentMapping(d, p, s, at)
		if e != nil {
			t.Fatal(e)
		}
		change(&p)
		if _, e = RecheckDocumentMapping(q, d, p, at); e == nil {
			t.Fatal("changed source snapshot reused")
		}
	}
	if _, e = ProposeDocumentMapping(d, p, s, time.Time{}); e == nil {
		t.Fatal("zero current time accepted")
	}
}

func TestDocumentMappingCopyIsolationAndNoReusablePermission(t *testing.T) {
	d, p, s, at := mappingFixture(t, "Exact source")
	q, e := ProposeDocumentMapping(d, p, s, at)
	if e != nil {
		t.Fatal(e)
	}
	before := q.Digest()
	copy, _ := q.CopyCandidate()
	copy.Objects[0][0] = '!'
	copy.Tags = append(copy.Tags, "changed")
	provenance := q.CopyProvenance()
	provenance[0] = '!'
	if _, e = RecheckDocumentMapping(q, d, p, at); e != nil || q.Digest() != before {
		t.Fatal("accessor alias mutated proposal", e)
	}
	p.Asset.Rights.AllowedUses[0] = "NONE"
	if _, e = RecheckDocumentMapping(q, d, p, at); e == nil {
		t.Fatal("later source edit ignored")
	}
	var zero DocumentMappingProposal
	if _, e = zero.CopyCandidate(); e == nil {
		t.Fatal("zero proposal accepted")
	}
	if _, e = RecheckDocumentMapping(zero, d, p, at); e == nil {
		t.Fatal("zero proposal rechecked")
	}
	q.candidate.Description = "Tampered internal proposal"
	d, p, _, at = mappingFixture(t, "Exact source")
	if _, e = RecheckDocumentMapping(q, d, p, at); e == nil {
		t.Fatal("tampered proposal candidate accepted")
	}
}

func TestDocumentMappingMetadataPreflightBeforeEncoding(t *testing.T) {
	d, p, s, at := mappingFixture(t, "Exact source")
	p.Asset.Name = strings.Repeat("x", (1<<20)+1)
	if _, e := ProposeDocumentMapping(d, p, s, at); e == nil {
		t.Fatal("oversized source metadata accepted")
	}
	d, p, s, at = mappingFixture(t, "Exact source")
	d.Input.Description = strings.Repeat("x", (1<<20)+1)
	if _, e := ProposeDocumentMapping(d, p, s, at); e == nil {
		t.Fatal("oversized target metadata accepted")
	}
	d, p, s, at = mappingFixture(t, "Exact source")
	d.Input.Tags = make([]string, 100001)
	if _, e := ProposeDocumentMapping(d, p, s, at); e == nil {
		t.Fatal("unbounded metadata width accepted")
	}
	s.ContentHash = strings.Repeat("x", (1<<20)+1)
	if _, _, e := mappingSnapshot(s, ""); e == nil {
		t.Fatal("selection escaped preflight")
	}
	for _, value := range []string{strings.Repeat("x", 900000), strings.Repeat("<", 150000)} {
		if _, _, e := mappingSnapshot(value, ""); e != nil {
			t.Fatal("independently bounded snapshot refused", e)
		}
	}
	if _, _, e := mappingSnapshot(strings.Repeat("<", 180000), ""); e == nil {
		t.Fatal("encoded escape expansion not bounded")
	}
}
