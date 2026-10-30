package documents

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"io"
	"path"
	"strconv"
	"strings"
	"unicode/utf8"
)

const wordNS = "http://schemas.openxmlformats.org/wordprocessingml/2006/main"
const relNS = "http://schemas.openxmlformats.org/package/2006/relationships"
const contentNS = "http://schemas.openxmlformats.org/package/2006/content-types"
const docxType = "application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"
const officeRel = "http://schemas.openxmlformats.org/officeDocument/2006/relationships/"

func attr(s xml.StartElement, name string) string {
	for _, a := range s.Attr {
		if a.Name.Local == name && a.Name.Space == "" {
			return a.Value
		}
	}
	return ""
}
func wordAttr(s xml.StartElement, name string) string {
	for _, a := range s.Attr {
		if a.Name.Local == name && a.Name.Space == wordNS {
			return a.Value
		}
	}
	return ""
}

// Decompression is bounded independently from attacker-controlled ZIP sizes.
// Duplicate/case-alias paths and unapproved relationship types fail closed.
func (x *extraction) docx(raw []byte) error {
	z, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return refused("DOCX_ZIP")
	}
	if len(z.File) > MaxZIPEntries {
		return refused("ZIP_ENTRIES")
	}
	parts := map[string][]byte{}
	seen := map[string]bool{}
	for _, f := range z.File {
		if err = x.tick(); err != nil {
			return err
		}
		name := f.Name
		fold := strings.ToLower(name)
		if name == "" || name == "." || name == ".." || strings.HasPrefix(name, "../") || strings.ContainsAny(name, "\\\x00:") || strings.HasPrefix(name, "/") || path.Clean(name) != strings.TrimSuffix(name, "/") || seen[fold] || !f.Mode().IsRegular() && !f.FileInfo().IsDir() {
			return refused("ZIP_PATH")
		}
		seen[fold] = true
		if f.FileInfo().IsDir() {
			continue
		}
		if strings.Contains(fold, "vbaproject") || strings.Contains(fold, "embeddings/") || strings.Contains(fold, "activex/") || strings.Contains(fold, "customui/") {
			return refused("DOCX_ACTIVE_CONTENT")
		}
		if f.CompressedSize64 > uint64(len(raw)) || f.UncompressedSize64 > MaxMemberBytes || f.UncompressedSize64 > uint64(MaxExpandedBytes-x.expanded) || f.UncompressedSize64 > f.CompressedSize64*MaxRatio {
			return refused("ZIP_EXPANSION")
		}
		r, e := f.Open()
		if e != nil {
			return refused("DOCX_ZIP")
		}
		b, e := io.ReadAll(io.LimitReader(r, MaxMemberBytes+1))
		closeErr := r.Close()
		if e != nil || closeErr != nil || len(b) > MaxMemberBytes || uint64(len(b)) != f.UncompressedSize64 {
			return refused("DOCX_ZIP")
		}
		x.expanded += len(b)
		if x.expanded > MaxExpandedBytes {
			return refused("ZIP_EXPANSION")
		}
		parts[name] = b
	}
	ct, ok := parts["[Content_Types].xml"]
	if !ok {
		return refused("DOCX_PACKAGE")
	}
	docType := false
	declarations := map[string]bool{}
	if err = x.xmlWalk(ct, func(t xml.Token, depth int) error {
		if s, ok := t.(xml.StartElement); ok {
			if depth == 1 && s.Name != (xml.Name{Space: contentNS, Local: "Types"}) {
				return refused("DOCX_PACKAGE")
			}
			if s.Name.Space != contentNS {
				return refused("DOCX_PACKAGE")
			}
			for _, a := range s.Attr {
				if a.Name.Space != "" && (a.Name.Local == "PartName" || a.Name.Local == "ContentType" || a.Name.Local == "Extension") {
					return refused("DOCX_PACKAGE")
				}
			}
			if depth > 1 && (depth != 2 || s.Name.Local != "Override" && s.Name.Local != "Default") {
				return refused("DOCX_PACKAGE")
			}
			if depth == 2 {
				key := attr(s, "PartName")
				if s.Name.Local == "Default" {
					key = "extension:" + attr(s, "Extension")
				}
				if key == "" || key == "extension:" || declarations[key] || attr(s, "ContentType") == "" {
					return refused("DOCX_PACKAGE")
				}
				declarations[key] = true
			}
			if s.Name.Local == "Override" && attr(s, "PartName") == "/word/document.xml" && attr(s, "ContentType") == docxType {
				docType = true
			}
			lower := strings.ToLower(attr(s, "ContentType"))
			if strings.Contains(lower, "macro") || strings.Contains(lower, "oleobject") || strings.Contains(lower, "activex") {
				return refused("DOCX_ACTIVE_CONTENT")
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if !docType {
		return refused("DOCX_PACKAGE")
	}
	rootFound := false
	for name, b := range parts {
		if strings.HasSuffix(name, ".rels") {
			relationships := map[string]bool{}
			if err = x.xmlWalk(b, func(t xml.Token, depth int) error {
				if s, ok := t.(xml.StartElement); ok {
					if s.Name.Space != relNS || depth == 1 && s.Name.Local != "Relationships" {
						return refused("DOCX_RELATIONSHIP")
					}
					if s.Name.Local != "Relationship" {
						if depth != 1 {
							return refused("DOCX_RELATIONSHIP")
						}
						return nil
					}
					if depth != 2 {
						return refused("DOCX_RELATIONSHIP")
					}
					for _, a := range s.Attr {
						if a.Name.Space != "" && (a.Name.Local == "Id" || a.Name.Local == "Type" || a.Name.Local == "Target" || a.Name.Local == "TargetMode") {
							return refused("DOCX_RELATIONSHIP")
						}
					}
					id := attr(s, "Id")
					if id == "" || relationships[id] {
						return refused("DOCX_RELATIONSHIP")
					}
					relationships[id] = true
					target, kind := attr(s, "Target"), attr(s, "Type")
					if attr(s, "TargetMode") != "" && attr(s, "TargetMode") != "Internal" || strings.ContainsAny(target, "\\\x00:") || strings.HasPrefix(target, "/") || path.Clean(target) != target || strings.HasPrefix(target, "../") {
						return refused("DOCX_EXTERNAL_RELATIONSHIP")
					}
					if kind == relNS+"/metadata/core-properties" {
						return nil
					}
					if !strings.HasPrefix(kind, officeRel) {
						return refused("DOCX_ACTIVE_CONTENT")
					}
					short := strings.TrimPrefix(kind, officeRel)
					switch short {
					case "officeDocument", "styles", "numbering", "settings", "webSettings", "fontTable", "theme", "image", "header", "footer", "footnotes", "endnotes", "comments", "hyperlink", "extended-properties", "custom-properties":
					default:
						return refused("DOCX_ACTIVE_CONTENT")
					}
					if name == "_rels/.rels" && short == "officeDocument" {
						if rootFound || target != "word/document.xml" {
							return refused("DOCX_PACKAGE")
						}
						rootFound = true
					}
				}
				return nil
			}); err != nil {
				return err
			}
		}
		if strings.HasPrefix(name, "word/") && (strings.HasPrefix(name, "word/header") || strings.HasPrefix(name, "word/footer") || strings.HasPrefix(name, "word/footnotes") || strings.HasPrefix(name, "word/endnotes") || strings.HasPrefix(name, "word/comments")) {
			if err = x.missing(name, "UNPROCESSED_PART"); err != nil {
				return err
			}
		}
	}
	if !rootFound {
		return refused("DOCX_PACKAGE")
	}
	body, ok := parts["word/document.xml"]
	if !ok {
		return refused("DOCX_PACKAGE")
	}
	paragraph, paragraphDepth, textDepth, suppress, bodyDepth := 0, 0, 0, 0, 0
	var text strings.Builder
	section := ""
	hidden := false
	err = x.xmlWalk(body, func(t xml.Token, depth int) error {
		switch s := t.(type) {
		case xml.StartElement:
			if depth == 1 && s.Name != (xml.Name{Space: wordNS, Local: "document"}) {
				return refused("DOCX_NAMESPACE")
			}
			if s.Name.Space == wordNS && s.Name.Local == "body" {
				if bodyDepth != 0 {
					return refused("DOCX_BODY")
				}
				bodyDepth = depth
			}
			if bodyDepth == 0 {
				return nil
			}
			if suppress > 0 {
				return nil
			}
			location := "word/document.xml:paragraph:" + strconv.Itoa(paragraph)
			if s.Name.Space != wordNS {
				if err := x.missing(location, "UNSUPPORTED_TEXT_CONTAINER"); err != nil {
					return err
				}
				suppress = depth
				return nil
			}
			switch s.Name.Local {
			case "p":
				if paragraphDepth != 0 {
					return refused("DOCX_PARAGRAPH")
				}
				paragraph++
				paragraphDepth = depth
				hidden = false
				text.Reset()
			case "t":
				if paragraphDepth > 0 {
					textDepth = depth
				} else if err := x.missing(location, "TEXT_OUTSIDE_PARAGRAPH"); err != nil {
					return err
				}
			case "tab":
				if paragraphDepth > 0 {
					return x.append(&text, "\t")
				}
			case "br", "cr":
				if paragraphDepth > 0 {
					return x.append(&text, "\n")
				}
			case "pStyle":
				if strings.HasPrefix(strings.ToLower(wordAttr(s, "val")), "heading") {
					section = wordAttr(s, "val")
				}
			case "vanish", "webHidden":
				hidden = true
				if err := x.missing(location, "HIDDEN_TEXT"); err != nil {
					return err
				}
			case "drawing", "pict", "object", "altChunk", "del", "ins", "instrText", "fldSimple", "fldChar", "txbxContent", "sdt", "sym":
				if err := x.missing(location, "UNPROCESSED_CONTAINER"); err != nil {
					return err
				}
				suppress = depth
			case "document", "body", "tbl", "tr", "tc", "pPr", "r", "rPr", "tblPr", "tblGrid", "gridCol", "tcPr", "trPr", "sectPr", "spacing", "jc", "ind", "keepNext", "keepLines", "outlineLvl", "rFonts", "b", "i", "u", "color", "sz", "szCs", "lang", "vertAlign", "pgSz", "pgMar", "numPr", "ilvl", "numId", "bookmarkStart", "bookmarkEnd", "proofErr", "hyperlink", "lastRenderedPageBreak":
			default:
				if err := x.missing(location, "UNPROCESSED_CONTAINER"); err != nil {
					return err
				}
				suppress = depth
			}
		case xml.CharData:
			if suppress == 0 && textDepth > 0 {
				if !utf8.Valid(s) {
					return refused("DOCX_UTF8")
				}
				return x.append(&text, string(s))
			}
		case xml.EndElement:
			if suppress == depth {
				suppress = 0
			}
			if textDepth == depth {
				textDepth = 0
			}
			if paragraphDepth == depth && s.Name.Space == wordNS && s.Name.Local == "p" {
				if !hidden {
					if err := x.add(Chunk{Location: "word/document.xml:paragraph:" + strconv.Itoa(paragraph), Text: text.String(), Paragraph: paragraph, Section: section}); err != nil {
						return err
					}
				}
				paragraphDepth = 0
			}
			if bodyDepth == depth && s.Name.Space == wordNS && s.Name.Local == "body" {
				bodyDepth = -1
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if bodyDepth != -1 {
		return refused("DOCX_BODY")
	}
	return nil
}

func (x *extraction) xmlWalk(raw []byte, visit func(xml.Token, int) error) error {
	if !utf8.Valid(raw) {
		return refused("DOCX_UTF8")
	}
	d := xml.NewDecoder(bytes.NewReader(raw))
	depth, roots := 0, 0
	for {
		if err := x.tick(); err != nil {
			return err
		}
		t, err := d.Token()
		if err == io.EOF {
			if depth != 0 || roots != 1 {
				return refused("DOCX_XML")
			}
			return nil
		}
		if err != nil {
			return refused("DOCX_XML")
		}
		switch s := t.(type) {
		case xml.StartElement:
			attributes := map[xml.Name]bool{}
			for _, a := range s.Attr {
				if attributes[a.Name] {
					return refused("DOCX_DUPLICATE_ATTRIBUTE")
				}
				attributes[a.Name] = true
			}
			if depth == 0 {
				roots++
			}
			depth++
			if depth > MaxDepth {
				return refused("XML_DEPTH")
			}
		case xml.Directive:
			return refused("XML_DIRECTIVE")
		case xml.ProcInst:
			if s.Target != "xml" {
				return refused("XML_DIRECTIVE")
			}
		case xml.CharData:
			if depth == 0 && strings.TrimSpace(string(s)) != "" {
				return refused("DOCX_XML")
			}
		}
		if err = visit(t, depth); err != nil {
			return err
		}
		if _, ok := t.(xml.EndElement); ok {
			depth--
		}
	}
}
