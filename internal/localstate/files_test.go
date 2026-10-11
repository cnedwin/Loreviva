package localstate

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAppendRefusesUnreadableGrowth(t *testing.T) {
	dir := t.TempDir()
	r, e := Open(dir, true)
	if e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(dir, "journal.jsonl")
	f, e := os.Create(path)
	if e != nil {
		t.Fatal(e)
	}
	if e = f.Truncate(32 << 20); e != nil {
		t.Fatal(e)
	}
	f.Close()
	if e = r.Append("journal.jsonl", []byte("x")); e == nil {
		t.Fatal("append crossed supported read bound")
	}
	b, e := r.Read("journal.jsonl")
	if e != nil || len(b) != 32<<20 {
		t.Fatal("old supported file damaged", len(b), e)
	}
}
