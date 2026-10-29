package documents

import (
	"bytes"
	"compress/zlib"
	"errors"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
)

// This is an intentionally small text-PDF reader, not a PDF renderer. Unknown
// representations remain visible gaps instead of guessed Unicode/prose.
type pdfName string
type pdfString []byte
type pdfRef struct{ id, generation int }
type pdfDict map[string]any
type pdfObject struct {
	value  any
	stream []byte
}
type pdfUnsupported struct{ code string }

func (e pdfUnsupported) Error() string { return e.code }

type pdfToken struct {
	kind, text string
	raw        []byte
}
type pdfLexer struct {
	raw []byte
	pos int
	x   *extraction
}

func pdfSpace(c byte) bool     { return c == 0 || c == 9 || c == 10 || c == 12 || c == 13 || c == 32 }
func pdfDelimiter(c byte) bool { return pdfSpace(c) || strings.ContainsRune("()<>[]{}/%", rune(c)) }
func hexNibble(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	}
	return 0, false
}
func (l *pdfLexer) next() (pdfToken, error) {
	if err := l.x.tick(); err != nil {
		return pdfToken{}, err
	}
	for l.pos < len(l.raw) {
		if pdfSpace(l.raw[l.pos]) {
			l.pos++
			continue
		}
		if l.raw[l.pos] == '%' {
			for l.pos < len(l.raw) && l.raw[l.pos] != '\n' && l.raw[l.pos] != '\r' {
				l.pos++
			}
			continue
		}
		break
	}
	if l.pos == len(l.raw) {
		return pdfToken{kind: "eof"}, nil
	}
	c := l.raw[l.pos]
	l.pos++
	switch c {
	case '[', ']':
		return pdfToken{kind: string(c)}, nil
	case '<':
		if l.pos < len(l.raw) && l.raw[l.pos] == '<' {
			l.pos++
			return pdfToken{kind: "<<"}, nil
		}
		out := []byte{}
		var nib byte
		have := false
		for l.pos < len(l.raw) {
			c = l.raw[l.pos]
			l.pos++
			if c == '>' {
				if have {
					out = append(out, nib<<4)
				}
				return pdfToken{kind: "string", raw: out}, nil
			}
			if pdfSpace(c) {
				continue
			}
			n, ok := hexNibble(c)
			if !ok {
				return pdfToken{}, refused("PDF_HEX")
			}
			if have {
				out = append(out, nib<<4|n)
				have = false
			} else {
				nib = n
				have = true
			}
			if len(out) > MaxOutputBytes {
				return pdfToken{}, refused("OUTPUT_LIMIT")
			}
		}
		return pdfToken{}, refused("PDF_STRING")
	case '>':
		if l.pos < len(l.raw) && l.raw[l.pos] == '>' {
			l.pos++
			return pdfToken{kind: ">>"}, nil
		}
		return pdfToken{}, refused("PDF_TOKEN")
	case '(':
		depth := 1
		out := []byte{}
		for l.pos < len(l.raw) {
			if err := l.x.tick(); err != nil {
				return pdfToken{}, err
			}
			c = l.raw[l.pos]
			l.pos++
			if c == '\\' {
				if l.pos == len(l.raw) {
					return pdfToken{}, refused("PDF_STRING")
				}
				c = l.raw[l.pos]
				l.pos++
				switch c {
				case 'n':
					c = '\n'
				case 'r':
					c = '\r'
				case 't':
					c = '\t'
				case 'b':
					c = '\b'
				case 'f':
					c = '\f'
				case '\n':
					continue
				case '\r':
					if l.pos < len(l.raw) && l.raw[l.pos] == '\n' {
						l.pos++
					}
					continue
				default:
					if c >= '0' && c <= '7' {
						v := int(c - '0')
						for i := 0; i < 2 && l.pos < len(l.raw) && l.raw[l.pos] >= '0' && l.raw[l.pos] <= '7'; i++ {
							v = v*8 + int(l.raw[l.pos]-'0')
							l.pos++
						}
						c = byte(v)
					}
				}
			} else if c == '(' {
				depth++
				if depth > MaxDepth {
					return pdfToken{}, refused("PDF_DEPTH")
				}
			} else if c == ')' {
				depth--
				if depth == 0 {
					return pdfToken{kind: "string", raw: out}, nil
				}
			}
			out = append(out, c)
			if len(out) > MaxOutputBytes {
				return pdfToken{}, refused("OUTPUT_LIMIT")
			}
		}
		return pdfToken{}, refused("PDF_STRING")
	case '/':
		var b strings.Builder
		for l.pos < len(l.raw) && !pdfDelimiter(l.raw[l.pos]) {
			c = l.raw[l.pos]
			l.pos++
			if c == '#' {
				if l.pos+1 >= len(l.raw) {
					return pdfToken{}, refused("PDF_NAME")
				}
				a, ok := hexNibble(l.raw[l.pos])
				d, ok2 := hexNibble(l.raw[l.pos+1])
				if !ok || !ok2 {
					return pdfToken{}, refused("PDF_NAME")
				}
				c = a<<4 | d
				l.pos += 2
			}
			b.WriteByte(c)
			if b.Len() > 256 {
				return pdfToken{}, refused("PDF_NAME")
			}
		}
		return pdfToken{kind: "name", text: b.String()}, nil
	default:
		start := l.pos - 1
		for l.pos < len(l.raw) && !pdfDelimiter(l.raw[l.pos]) {
			l.pos++
		}
		if start == l.pos {
			return pdfToken{}, refused("PDF_TOKEN")
		}
		if l.pos-start > 256 {
			return pdfToken{}, refused("PDF_TOKEN")
		}
		s := string(l.raw[start:l.pos])
		if _, e := strconv.ParseFloat(s, 64); e == nil {
			return pdfToken{kind: "number", text: s}, nil
		}
		return pdfToken{kind: "word", text: s}, nil
	}
}
func (l *pdfLexer) value(depth int) (any, error) {
	t, e := l.next()
	if e != nil {
		return nil, e
	}
	return l.from(t, depth)
}
func (l *pdfLexer) from(t pdfToken, depth int) (any, error) {
	if depth > MaxDepth {
		return nil, refused("PDF_DEPTH")
	}
	switch t.kind {
	case "name":
		return pdfName(t.text), nil
	case "string":
		return pdfString(t.raw), nil
	case "number":
		n, e := strconv.Atoi(t.text)
		if e == nil && n >= 0 {
			saved := l.pos
			t2, e2 := l.next()
			if e2 != nil {
				return nil, e2
			}
			if t2.kind == "number" {
				g, e := strconv.Atoi(t2.text)
				if e == nil && g >= 0 {
					t3, e := l.next()
					if e != nil {
						return nil, e
					}
					if t3.kind == "word" && t3.text == "R" {
						return pdfRef{n, g}, nil
					}
				}
			}
			l.pos = saved
		}
		return t.text, nil
	case "[":
		out := []any{}
		for {
			t, e := l.next()
			if e != nil {
				return nil, e
			}
			if t.kind == "]" {
				return out, nil
			}
			if len(out) >= MaxPDFObjects {
				return nil, refused("PDF_ARRAY")
			}
			v, e := l.from(t, depth+1)
			if e != nil {
				return nil, e
			}
			out = append(out, v)
		}
	case "<<":
		out := pdfDict{}
		for {
			t, e := l.next()
			if e != nil {
				return nil, e
			}
			if t.kind == ">>" {
				return out, nil
			}
			if t.kind != "name" || len(out) >= MaxPDFObjects {
				return nil, refused("PDF_DICTIONARY")
			}
			if _, ok := out[t.text]; ok {
				return nil, refused("PDF_DUPLICATE_KEY")
			}
			v, e := l.value(depth + 1)
			if e != nil {
				return nil, e
			}
			out[t.text] = v
		}
	case "word":
		if t.text == "null" || t.text == "true" || t.text == "false" {
			return t.text, nil
		}
	}
	return nil, refused("PDF_VALUE")
}

type pdfDocument struct {
	x       *extraction
	objects map[pdfRef]pdfObject
	catalog pdfDict
}

func (x *extraction) pdf(raw []byte) error {
	if !bytes.HasPrefix(raw, []byte("%PDF-1.")) && !bytes.HasPrefix(raw, []byte("%PDF-2.")) {
		return refused("PDF_SIGNATURE")
	}
	p := &pdfDocument{x: x, objects: map[pdfRef]pdfObject{}}
	err := p.inspect(raw)
	var unsupported pdfUnsupported
	if errors.As(err, &unsupported) {
		x.report.Chunks = []Chunk{}
		x.report.Status = "UNSUPPORTED"
		return x.missing("document", unsupported.code)
	}
	return err
}
func (p *pdfDocument) inspect(raw []byte) error {
	l := pdfLexer{raw: raw, x: p.x}
	offsets := map[pdfRef]int{}
	boundRoot := false
	for {
		t, e := l.next()
		if e != nil {
			return e
		}
		if t.kind == "eof" {
			break
		}
		if t.kind == "word" && t.text == "xref" {
			if e = p.readXref(&l, l.pos-4, offsets); e != nil {
				return e
			}
			boundRoot = true
			break
		}
		if t.kind == "word" && t.text == "trailer" {
			return refused("PDF_XREF")
		}
		if t.kind != "number" {
			continue
		}
		objectOffset := l.pos - len(t.text)
		id, e := strconv.Atoi(t.text)
		if e != nil || id < 1 {
			continue
		}
		t2, e := l.next()
		if e != nil {
			return e
		}
		generation, e := strconv.Atoi(t2.text)
		if t2.kind != "number" || e != nil || generation < 0 {
			continue
		}
		t3, e := l.next()
		if e != nil {
			return e
		}
		if t3.kind != "word" || t3.text != "obj" {
			continue
		}
		ref := pdfRef{id, generation}
		if _, ok := p.objects[ref]; ok {
			return pdfUnsupported{"PDF_INCREMENTAL_UPDATE"}
		}
		if len(p.objects) >= MaxPDFObjects {
			return refused("PDF_OBJECT_LIMIT")
		}
		v, e := l.value(0)
		if e != nil {
			return e
		}
		o := pdfObject{value: v}
		tail, e := l.next()
		if e != nil {
			return e
		}
		if tail.kind == "word" && tail.text == "stream" {
			d, ok := v.(pdfDict)
			if !ok {
				return refused("PDF_STREAM")
			}
			if d["Type"] == pdfName("ObjStm") {
				return pdfUnsupported{"PDF_OBJECT_STREAM"}
			}
			length, ok := d["Length"].(string)
			if !ok {
				return pdfUnsupported{"PDF_INDIRECT_STREAM_LENGTH"}
			}
			n, e := strconv.Atoi(length)
			if e != nil || n < 0 || n > MaxMemberBytes {
				return refused("PDF_STREAM_LIMIT")
			}
			if l.pos < len(raw) && raw[l.pos] == '\r' {
				l.pos++
				if l.pos < len(raw) && raw[l.pos] == '\n' {
					l.pos++
				}
			} else if l.pos < len(raw) && raw[l.pos] == '\n' {
				l.pos++
			} else {
				return refused("PDF_STREAM")
			}
			if n > len(raw)-l.pos {
				return refused("PDF_STREAM")
			}
			o.stream = raw[l.pos : l.pos+n]
			l.pos += n
			tail, e = l.next()
			if e != nil {
				return e
			}
			if tail.kind != "word" || tail.text != "endstream" {
				return refused("PDF_STREAM")
			}
			tail, e = l.next()
			if e != nil {
				return e
			}
		}
		if tail.kind != "word" || tail.text != "endobj" {
			return refused("PDF_OBJECT")
		}
		p.objects[ref] = o
		offsets[ref] = objectOffset
		if d, ok := v.(pdfDict); ok && d["Type"] == pdfName("Catalog") {
			if p.catalog != nil {
				return pdfUnsupported{"PDF_MULTIPLE_CATALOGS"}
			}
			p.catalog = d
		}
	}
	if !boundRoot || p.catalog == nil {
		return refused("PDF_ROOT")
	}
	for _, key := range []string{"AcroForm", "Names", "OCProperties", "OpenAction", "AA"} {
		if _, ok := p.catalog[key]; ok {
			if e := p.x.missing("catalog", "PDF_OMITTED_"+strings.ToUpper(key)); e != nil {
				return e
			}
		}
	}
	root, ok := p.catalog["Pages"].(pdfRef)
	if !ok {
		return refused("PDF_PAGE_TREE")
	}
	page := 0
	seen := map[pdfRef]bool{}
	if e := p.pages(root, nil, 0, seen, &page); e != nil {
		return e
	}
	if page == 0 {
		return refused("PDF_PAGE_TREE")
	}
	if len(p.x.report.Chunks) == 0 {
		p.x.report.Status = "UNSUPPORTED"
		for _, gap := range p.x.report.Missing {
			if gap.Code == "PDF_IMAGE_REQUIRES_OCR" {
				p.x.report.Status = "OCR_REQUIRED"
			}
		}
		if e := p.x.missing("document", "PDF_NO_EXTRACTABLE_TEXT"); e != nil {
			return e
		}
	}
	return nil
}
func (p *pdfDocument) dict(v any) (pdfDict, bool) {
	if r, ok := v.(pdfRef); ok {
		v = p.objects[r].value
	}
	d, ok := v.(pdfDict)
	return d, ok
}
func (p *pdfDocument) pages(ref pdfRef, resources any, depth int, seen map[pdfRef]bool, page *int) error {
	if e := p.x.tick(); e != nil {
		return e
	}
	if depth > MaxDepth || seen[ref] {
		return refused("PDF_PAGE_CYCLE")
	}
	seen[ref] = true
	d, ok := p.dict(ref)
	if !ok {
		return refused("PDF_PAGE_TREE")
	}
	if r, ok := d["Resources"]; ok {
		resources = r
	}
	switch d["Type"] {
	case pdfName("Pages"):
		kids, ok := d["Kids"].([]any)
		if !ok || len(kids) == 0 {
			return refused("PDF_PAGE_TREE")
		}
		for _, v := range kids {
			r, ok := v.(pdfRef)
			if !ok {
				return refused("PDF_PAGE_TREE")
			}
			if e := p.pages(r, resources, depth+1, seen, page); e != nil {
				return e
			}
		}
	case pdfName("Page"):
		*page++
		if *page > MaxPages {
			return refused("PDF_PAGE_LIMIT")
		}
		return p.page(d, resources, *page)
	default:
		return refused("PDF_PAGE_TREE")
	}
	return nil
}
func (p *pdfDocument) stream(ref pdfRef) ([]byte, error) {
	o, ok := p.objects[ref]
	if !ok || o.stream == nil {
		return nil, refused("PDF_CONTENT_STREAM")
	}
	d, ok := o.value.(pdfDict)
	if !ok {
		return nil, refused("PDF_STREAM")
	}
	filter := d["Filter"]
	if a, ok := filter.([]any); ok {
		if len(a) != 1 {
			return nil, pdfUnsupported{"PDF_FILTER"}
		}
		filter = a[0]
	}
	if _, ok := d["DecodeParms"]; ok {
		return nil, pdfUnsupported{"PDF_DECODE_PARAMETERS"}
	}
	var b []byte
	if filter == nil {
		b = o.stream
	} else if filter == pdfName("FlateDecode") {
		r, e := zlib.NewReader(bytes.NewReader(o.stream))
		if e != nil {
			return nil, refused("PDF_FLATE")
		}
		b, e = io.ReadAll(io.LimitReader(r, MaxMemberBytes+1))
		closeErr := r.Close()
		if e != nil || closeErr != nil {
			return nil, refused("PDF_FLATE")
		}
		if len(b) > MaxMemberBytes || len(b) > MaxRatio*len(o.stream) {
			return nil, refused("PDF_EXPANSION")
		}
	} else {
		return nil, pdfUnsupported{"PDF_FILTER"}
	}
	p.x.expanded += len(b)
	if p.x.expanded > MaxExpandedBytes {
		return nil, refused("PDF_EXPANSION")
	}
	return b, nil
}
func (p *pdfDocument) font(resources pdfDict, name string) (string, bool) {
	fonts, ok := p.dict(resources["Font"])
	if !ok {
		return "", false
	}
	font, ok := p.dict(fonts[name])
	if !ok || font["Subtype"] != pdfName("Type1") {
		return "", false
	}
	if _, ok := font["ToUnicode"]; ok {
		return "", false
	}
	base, ok := font["BaseFont"].(pdfName)
	if !ok {
		return "", false
	}
	valid := false
	for _, n := range []string{"Helvetica", "Helvetica-Bold", "Helvetica-Oblique", "Helvetica-BoldOblique", "Times-Roman", "Times-Bold", "Times-Italic", "Times-BoldItalic", "Courier", "Courier-Bold", "Courier-Oblique", "Courier-BoldOblique"} {
		if string(base) == n {
			valid = true
		}
	}
	if !valid {
		return "", false
	}
	if font["Encoding"] == nil || font["Encoding"] == pdfName("StandardEncoding") {
		return "ASCII", true
	}
	if font["Encoding"] == pdfName("WinAnsiEncoding") {
		return "WINANSI", true
	}
	return "", false
}
func (p *pdfDocument) page(d pdfDict, resource any, page int) error {
	location := "page:" + strconv.Itoa(page)
	resources, ok := p.dict(resource)
	if !ok {
		if e := p.x.missing(location, "PDF_RESOURCES"); e != nil {
			return e
		}
		resources = pdfDict{}
	}
	for _, k := range []string{"Annots", "AA", "Thumb"} {
		if _, ok := d[k]; ok {
			if e := p.x.missing(location, "PDF_OMITTED_"+strings.ToUpper(k)); e != nil {
				return e
			}
		}
	}
	content := d["Contents"]
	refs := []any{}
	if content != nil {
		if a, ok := content.([]any); ok {
			refs = a
		} else {
			refs = []any{content}
		}
	}
	var body strings.Builder
	joined := []byte{}
	for _, r := range refs {
		ref, ok := r.(pdfRef)
		if !ok {
			return refused("PDF_CONTENT_STREAM")
		}
		b, e := p.stream(ref)
		if e != nil {
			var u pdfUnsupported
			if errors.As(e, &u) {
				if e = p.x.missing(location, u.code); e != nil {
					return e
				}
				return nil
			}
			return e
		}
		if len(b)+len(joined) > MaxExpandedBytes {
			return refused("PDF_EXPANSION")
		}
		joined = append(joined, b...)
	}
	if e := p.content(joined, resources, location, &body); e != nil {
		return e
	}
	if strings.TrimSpace(body.String()) == "" {
		return p.x.missing(location, "PDF_PAGE_NEEDS_OCR_OR_UNSUPPORTED_TEXT")
	}
	return p.x.add(Chunk{Location: location, Text: body.String(), Page: page})
}
func (p *pdfDocument) content(raw []byte, resources pdfDict, location string, body *strings.Builder) error {
	l := pdfLexer{raw: raw, x: p.x}
	args := []any{}
	inText := false
	encoding := ""
	supported := false
	type fontState struct {
		encoding  string
		supported bool
	}
	stack := []fontState{}
	show := func(v any) error {
		b, ok := v.(pdfString)
		if !ok {
			return refused("PDF_TEXT_OPERATOR")
		}
		if !supported {
			return p.x.missing(location, "PDF_UNSUPPORTED_FONT")
		}
		var text string
		if encoding == "ASCII" {
			for _, c := range b {
				if c < 32 && c != '\t' && c != '\n' && c != '\r' || c >= 127 {
					return p.x.missing(location, "PDF_UNSUPPORTED_CHARACTER")
				}
			}
			text = string(b)
		} else {
			v, e := charmap.Windows1252.NewDecoder().Bytes(b)
			if e != nil || bytes.Contains(v, []byte("\xef\xbf\xbd")) {
				return p.x.missing(location, "PDF_UNSUPPORTED_CHARACTER")
			}
			text = string(v)
		}
		if !utf8.ValidString(text) {
			return refused("PDF_TEXT_ENCODING")
		}
		return p.x.append(body, text)
	}
	for {
		t, e := l.next()
		if e != nil {
			return e
		}
		if t.kind == "eof" {
			if inText || len(stack) > 0 {
				return refused("PDF_TEXT_STATE")
			}
			return nil
		}
		if t.kind != "word" {
			v, e := l.from(t, 0)
			if e != nil {
				return e
			}
			if len(args) >= MaxPDFObjects {
				return refused("PDF_OPERANDS")
			}
			args = append(args, v)
			continue
		}
		switch t.text {
		case "q":
			if len(stack) >= MaxDepth {
				return refused("PDF_GRAPHICS_DEPTH")
			}
			stack = append(stack, fontState{encoding, supported})
		case "Q":
			if len(stack) == 0 {
				return refused("PDF_GRAPHICS_STATE")
			}
			saved := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			encoding, supported = saved.encoding, saved.supported
		case "BT":
			if inText {
				return refused("PDF_TEXT_STATE")
			}
			inText = true
		case "ET":
			if !inText {
				return refused("PDF_TEXT_STATE")
			}
			inText = false
			if body.Len() > 0 {
				if e = p.x.append(body, "\n"); e != nil {
					return e
				}
			}
		case "Tf":
			if !inText || len(args) != 2 {
				return refused("PDF_TEXT_OPERATOR")
			}
			name, ok := args[0].(pdfName)
			if !ok {
				return refused("PDF_TEXT_OPERATOR")
			}
			encoding, supported = p.font(resources, string(name))
		case "Tj", "'", "\"":
			want := 1
			if t.text == "\"" {
				want = 3
			}
			if !inText || len(args) != want {
				return refused("PDF_TEXT_OPERATOR")
			}
			if t.text != "Tj" {
				if e = p.x.append(body, "\n"); e != nil {
					return e
				}
			}
			if e = show(args[len(args)-1]); e != nil {
				return e
			}
		case "TJ":
			if !inText || len(args) != 1 {
				return refused("PDF_TEXT_OPERATOR")
			}
			a, ok := args[0].([]any)
			if !ok {
				return refused("PDF_TEXT_OPERATOR")
			}
			for _, v := range a {
				if _, ok := v.(pdfString); ok {
					if e = show(v); e != nil {
						return e
					}
				} else if s, ok := v.(string); !ok {
					return refused("PDF_TEXT_OPERATOR")
				} else if _, e = strconv.ParseFloat(s, 64); e != nil {
					return refused("PDF_TEXT_OPERATOR")
				}
			}
		case "Td", "TD", "T*":
			if inText && body.Len() > 0 {
				if e = p.x.append(body, "\n"); e != nil {
					return e
				}
			}
		case "Do", "BI", "BDC", "BMC", "EMC", "sh", "gs":
			code := "PDF_UNPROCESSED_CONTENT"
			if t.text == "BI" {
				code = "PDF_IMAGE_REQUIRES_OCR"
			}
			if t.text == "Do" && len(args) == 1 {
				if name, ok := args[0].(pdfName); ok {
					if objects, ok := p.dict(resources["XObject"]); ok {
						if object, ok := p.dict(objects[string(name)]); ok && object["Subtype"] == pdfName("Image") {
							code = "PDF_IMAGE_REQUIRES_OCR"
						}
					}
				}
			}
			if e = p.x.missing(location, code); e != nil {
				return e
			}
			if t.text == "BI" {
				return nil
			}
		case "cm", "w", "J", "j", "M", "d", "ri", "i", "m", "l", "c", "v", "y", "h", "re", "S", "s", "f", "F", "f*", "B", "B*", "b", "b*", "n", "W", "W*", "CS", "cs", "SC", "SCN", "sc", "scn", "G", "g", "RG", "rg", "K", "k", "Tc", "Tw", "Tz", "TL", "Tr", "Ts", "Tm", "d0", "d1", "BX", "EX":
		default:
			if e = p.x.missing(location, "PDF_UNKNOWN_OPERATOR"); e != nil {
				return e
			}
		}
		args = args[:0]
	}
}
