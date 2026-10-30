// Package documents inspects untrusted local documents as inert source material.
// It never opens a path, resolves a URL, invokes a tool, or writes content.
package documents

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

const ParserVersion = "bounded-text-v1"
const MaxInputBytes = 8 << 20
const MaxExpandedBytes = 16 << 20
const MaxMemberBytes = 4 << 20
const MaxOutputBytes = 512 << 10

// Includes metadata, JSON escaping and the encoder's final newline. This
// matches the existing ordinary wire boundary; preview never raises it.
const MaxResponseBytes = 1 << 20
const MaxChunks = 1000
const MaxPages = 200
const MaxZIPEntries = 128
const MaxRatio = 100
const MaxDepth = 64
const MaxPDFObjects = 4096
const MaxTokens = 200000
const Timeout = 3 * time.Second

// Offsets refer to original UTF-8 bytes for TXT/MD. DOCX locations name the
// actual XML paragraph; PDF locations name the actual page-tree page.
type Chunk struct {
	Location  string `json:"location"`
	Text      string `json:"text"`
	SHA256    string `json:"sha256"`
	Line      int    `json:"line,omitempty"`
	ByteStart *int   `json:"byte_start,omitempty"`
	ByteEnd   *int   `json:"byte_end,omitempty"`
	Paragraph int    `json:"paragraph,omitempty"`
	Page      int    `json:"page,omitempty"`
	Section   string `json:"section,omitempty"`
}
type Missing struct {
	Location string `json:"location"`
	Code     string `json:"code"`
}
type Report struct {
	ParserVersion  string    `json:"parser_version"`
	Format         string    `json:"format"`
	SourceSHA256   string    `json:"source_sha256"`
	SizeBytes      string    `json:"size_bytes"`
	Status         string    `json:"status"`
	Chunks         []Chunk   `json:"chunks"`
	Missing        []Missing `json:"missing"`
	OCR            bool      `json:"ocr"`
	ExecutionReady bool      `json:"execution_ready"`
	Adoptable      bool      `json:"adoptable"`
}
type Error struct{ Code string }

func (e *Error) Error() string  { return "document refused: " + e.Code }
func refused(code string) error { return &Error{code} }
func digest(b []byte) string    { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

type extraction struct {
	ctx                      context.Context
	report                   Report
	output, tokens, expanded int
}

func (x *extraction) tick() error {
	if err := x.ctx.Err(); err != nil {
		return err
	}
	x.tokens++
	if x.tokens > MaxTokens {
		return refused("TOKEN_LIMIT")
	}
	return nil
}
func (x *extraction) missing(location, code string) error {
	if err := x.tick(); err != nil {
		return err
	}
	for _, v := range x.report.Missing {
		if v.Location == location && v.Code == code {
			return nil
		}
	}
	if len(x.report.Missing) >= MaxChunks {
		return refused("MISSING_LIMIT")
	}
	x.report.Missing = append(x.report.Missing, Missing{location, code})
	return nil
}
func (x *extraction) add(c Chunk) error {
	if err := x.tick(); err != nil {
		return err
	}
	if c.Text == "" {
		return nil
	}
	if len(x.report.Chunks) >= MaxChunks || len(c.Text) > MaxOutputBytes-x.output {
		return refused("OUTPUT_LIMIT")
	}
	x.output += len(c.Text)
	c.SHA256 = digest([]byte(c.Text))
	x.report.Chunks = append(x.report.Chunks, c)
	return nil
}
func (x *extraction) append(b *strings.Builder, s string) error {
	if err := x.tick(); err != nil {
		return err
	}
	if b.Len()+len(s) > MaxOutputBytes-x.output {
		return refused("OUTPUT_LIMIT")
	}
	b.WriteString(s)
	return nil
}

// Extract returns no report for malformed/unsafe inputs, exhausted limits or
// cancellation. A partial report is a successful inspection with explicit gaps,
// never a permission to adopt or publish that source.
func Extract(ctx context.Context, raw []byte, format string) (Report, error) {
	if ctx == nil {
		return Report{}, refused("CONTEXT_REQUIRED")
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	if len(raw) == 0 || len(raw) > MaxInputBytes {
		return Report{}, refused("INPUT_LIMIT")
	}
	x := &extraction{ctx: ctx, report: Report{ParserVersion: ParserVersion, Format: format, SourceSHA256: digest(raw), SizeBytes: fmtSize(len(raw)), Status: "COMPLETE", Chunks: []Chunk{}, Missing: []Missing{}}}
	var err error
	switch format {
	case "TXT", "MD":
		err = x.text(raw)
	case "DOCX":
		err = x.docx(raw)
	case "PDF":
		err = x.pdf(raw)
	default:
		err = refused("FORMAT_UNSUPPORTED")
	}
	if err != nil {
		return Report{}, err
	}
	if err = x.ctx.Err(); err != nil {
		return Report{}, err
	}
	if len(x.report.Missing) > 0 && x.report.Status == "COMPLETE" {
		x.report.Status = "PARTIAL"
	}
	return x.report, nil
}
