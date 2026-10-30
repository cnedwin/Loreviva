package documents

import (
	"bytes"
	"strconv"
)

// readXref binds the reader to the supported classic xref/trailer structure.
// It never chooses a convenient orphan Catalog or repairs corrupt offsets.
func (p *pdfDocument) readXref(l *pdfLexer, xrefOffset int, offsets map[pdfRef]int) error {
	entries := map[pdfRef]int{}
	seenIDs := map[int]bool{}
	counted := 0
	for {
		t, e := l.next()
		if e != nil {
			return e
		}
		if t.kind == "word" && t.text == "trailer" {
			break
		}
		start, e := strconv.Atoi(t.text)
		if t.kind != "number" || e != nil || start < 0 {
			return refused("PDF_XREF")
		}
		t, e = l.next()
		if e != nil {
			return e
		}
		count, e := strconv.Atoi(t.text)
		if t.kind != "number" || e != nil || count < 1 || count > MaxPDFObjects+1-counted || start > int(^uint(0)>>1)-count {
			return refused("PDF_XREF_LIMIT")
		}
		counted += count
		for i := 0; i < count; i++ {
			o, e := l.next()
			if e != nil {
				return e
			}
			g, e := l.next()
			if e != nil {
				return e
			}
			flag, e := l.next()
			if e != nil {
				return e
			}
			offset, oe := strconv.Atoi(o.text)
			generation, ge := strconv.Atoi(g.text)
			if o.kind != "number" || g.kind != "number" || oe != nil || ge != nil || offset < 0 || generation < 0 || generation > 65535 || flag.kind != "word" || flag.text != "n" && flag.text != "f" || seenIDs[start+i] {
				return refused("PDF_XREF")
			}
			seenIDs[start+i] = true
			if flag.text == "n" {
				ref := pdfRef{start + i, generation}
				actual, ok := offsets[ref]
				if !ok || actual != offset || offset >= len(l.raw) {
					return refused("PDF_XREF_OFFSET")
				}
				entries[ref] = offset
			}
		}
	}
	v, e := l.value(0)
	if e != nil {
		return e
	}
	trailer, ok := v.(pdfDict)
	if !ok {
		return refused("PDF_TRAILER")
	}
	if _, ok = trailer["Encrypt"]; ok {
		return pdfUnsupported{"PDF_ENCRYPTED"}
	}
	if _, ok = trailer["Prev"]; ok {
		return pdfUnsupported{"PDF_INCREMENTAL_UPDATE"}
	}
	if _, ok = trailer["XRefStm"]; ok {
		return pdfUnsupported{"PDF_HYBRID_XREF"}
	}
	root, ok := trailer["Root"].(pdfRef)
	if !ok {
		return refused("PDF_ROOT")
	}
	catalog, ok := p.dict(root)
	if !ok || catalog["Type"] != pdfName("Catalog") {
		return refused("PDF_ROOT")
	}
	if _, ok = entries[root]; !ok {
		return refused("PDF_ROOT")
	}
	if len(entries) != len(offsets) {
		return refused("PDF_XREF_OFFSET")
	}
	p.catalog = catalog
	t, e := l.next()
	if e != nil {
		return e
	}
	if t.kind != "word" || t.text != "startxref" {
		return refused("PDF_TRAILER")
	}
	t, e = l.next()
	if e != nil {
		return e
	}
	offset, e := strconv.Atoi(t.text)
	if t.kind != "number" || e != nil || offset != xrefOffset {
		return refused("PDF_XREF_OFFSET")
	}
	// Only whitespace may surround the final marker. An appended unindexed
	// object cannot silently change the document inspected by this subset.
	if !bytes.Equal(bytes.TrimSpace(l.raw[l.pos:]), []byte("%%EOF")) {
		return refused("PDF_EOF")
	}
	l.pos = len(l.raw)
	return nil
}
