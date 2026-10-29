package documents

import (
	"bytes"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

func fmtSize(n int) string { return strconv.Itoa(n) }
func (x *extraction) text(raw []byte) error {
	if !utf8.Valid(raw) || bytes.HasPrefix(raw, []byte("PK\x03\x04")) || bytes.HasPrefix(raw, []byte("%PDF-")) {
		return refused("TEXT_TYPE_MISMATCH")
	}
	for _, r := range string(raw) {
		if unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' {
			return refused("BINARY_TEXT")
		}
	}
	start, line := 0, 1
	section := ""
	for start < len(raw) {
		if err := x.tick(); err != nil {
			return err
		}
		end := start
		for end < len(raw) && raw[end] != '\n' {
			end++
		}
		if end < len(raw) {
			end++
		}
		text := string(raw[start:end])
		if x.report.Format == "MD" {
			head := strings.TrimSpace(text)
			if strings.HasPrefix(head, "#") {
				i := 0
				for i < len(head) && head[i] == '#' {
					i++
				}
				if i <= 6 && i < len(head) && head[i] == ' ' {
					section = strings.TrimSpace(head[i:])
					if len(section) > 300 {
						section = ""
					}
				}
			}
		}
		a, b := start, end
		if err := x.add(Chunk{Location: "line:" + strconv.Itoa(line), Text: text, Line: line, ByteStart: &a, ByteEnd: &b, Section: section}); err != nil {
			return err
		}
		start = end
		line++
	}
	return nil
}
