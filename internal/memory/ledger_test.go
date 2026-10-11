package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLedgerReservationSurvivesRestartAndPinsCurrentHead(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	l, e := CreateLedger(dir, "install", "ledger", "anchor")
	if e != nil {
		t.Fatal(e)
	}
	r := Reservation{ID: "original-task", RequestHash: "request-hash", Scope: []string{"tenant", "product", "world", "alex", "player"}}
	if e = l.ReserveOnce(ctx, r); e != nil {
		t.Fatal(e)
	}
	l, e = OpenLedger(dir, "install", "ledger", "anchor")
	if e != nil {
		t.Fatal(e)
	}
	if e = l.Ready(ctx); e == nil {
		t.Fatal("unresolved reservation opened readiness")
	}
	if e = l.AppendOnce(ctx, r.ID, Binding{SelectorHash: "selector", SourceEpoch: "epoch", Cutoff: 3, Targets: []string{"memory"}}); e != nil {
		t.Fatal(e)
	}
	h, e := l.LatestHead(ctx, "challenge-1")
	if e != nil {
		t.Fatal(e)
	}
	if e = l.Ready(ctx); e != nil {
		t.Fatal(e)
	}
	if e = l.VerifyHead(ctx, h); e != nil {
		t.Fatal(e)
	}
	r.ID = "task-2"
	if e = l.ReserveOnce(ctx, r); e != nil {
		t.Fatal(e)
	}
	if e = l.VerifyHead(ctx, h); e == nil {
		t.Fatal("stale proof accepted after fresh reservation")
	}
}
func TestLedgerRejectsJournalRollbackAndMissingAnchor(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	l, e := CreateLedger(dir, "i", "l", "a")
	if e != nil {
		t.Fatal(e)
	}
	old, e := os.ReadFile(filepath.Join(dir, "journal.jsonl"))
	if e != nil {
		t.Fatal(e)
	}
	if e = l.ReserveOnce(ctx, Reservation{ID: "task", RequestHash: "hash", Scope: []string{"t", "p", "w", "a", "s"}}); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(dir, "journal.jsonl"), old, 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = l.LatestHead(ctx, "fresh"); e == nil {
		t.Fatal("current anchor accepted rolled back journal")
	}
	if e = os.Remove(filepath.Join(dir, "anchor.json")); e != nil {
		t.Fatal(e)
	}
	if _, e = OpenLedger(dir, "i", "l", "a"); e == nil {
		t.Fatal("missing anchor silently recreated")
	}
}

func TestLedgerTwoProcessAppendSerializes(t *testing.T) {
	dir := t.TempDir()
	l, e := CreateLedger(dir, "installation", "ledger", "anchor")
	if e != nil {
		t.Fatal(e)
	}
	commands := []*exec.Cmd{}
	for _, prefix := range []string{"one", "two"} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestLedgerProcessHelper$")
		cmd.Env = append(os.Environ(), "LOREVIVA_LEDGER_HELPER="+dir, "LOREVIVA_LEDGER_PREFIX="+prefix)
		commands = append(commands, cmd)
		if e = cmd.Start(); e != nil {
			t.Fatal(e)
		}
	}
	for _, cmd := range commands {
		if e = cmd.Wait(); e != nil {
			t.Fatal(e)
		}
	}
	h, e := l.LatestHead(context.Background(), "fresh-parent")
	if e != nil {
		t.Fatal(e)
	}
	if h.Sequence != 40 {
		t.Fatalf("lost cross-process append: %d", h.Sequence)
	}
	if e = l.Ready(context.Background()); e != nil {
		t.Fatal(e)
	}
}
func TestLedgerProcessHelper(t *testing.T) {
	dir := os.Getenv("LOREVIVA_LEDGER_HELPER")
	if dir == "" {
		t.Skip("child process only")
	}
	l, e := OpenLedger(dir, "installation", "ledger", "anchor")
	if e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 10; i++ {
		id := fmt.Sprintf("%s-%d", os.Getenv("LOREVIVA_LEDGER_PREFIX"), i)
		r := Reservation{ID: id, RequestHash: "hash", Scope: []string{"t", "p", "w", "subject", "caller"}}
		if e = l.ReserveOnce(context.Background(), r); e != nil {
			t.Fatal(e)
		}
		if e = l.AppendOnce(context.Background(), id, Binding{SelectorHash: "selector", SourceEpoch: "epoch", Cutoff: 1, Targets: []string{"node"}}); e != nil {
			t.Fatal(e)
		}
	}
}

func TestLedgerCapacityReservesPendingCompletionBeforeG1(t *testing.T) {
	ctx := context.Background()
	l, e := CreateLedger(t.TempDir(), "install", "ledger", "anchor")
	if e != nil {
		t.Fatal(e)
	}
	pending := Reservation{ID: "pending", RequestHash: "hash", Scope: []string{"t", "p", "w", "s", "c"}}
	if e = l.ReserveOnce(ctx, pending); e != nil {
		t.Fatal(e)
	}
	a, entries, e := l.load()
	if e != nil {
		t.Fatal(e)
	}
	raw, e := l.root.Read("journal.jsonl")
	if e != nil {
		t.Fatal(e)
	}
	buffer := bytes.NewBuffer(raw)
	// Build a valid retained history near the declared capacity, without hundreds
	// of fsync cycles. Every completion stays below its reserved 512 KiB bound.
	large := Binding{SourceEpoch: "epoch", SelectorHash: "hash", Action: "DELETE", TargetKind: "SOURCE"}
	for j := 0; j < 1000; j++ {
		large.Targets = append(large.Targets, fmt.Sprintf("%04d%s", j, strings.Repeat("x", 60)))
	}
	appendEntry := func(v LedgerEntry) {
		v.Sequence = a.Sequence + 1
		v.PrevHash = a.Hash
		v.Hash = digest(v)
		b, _ := json.Marshal(v)
		buffer.Write(b)
		buffer.WriteByte('\n')
		a.Sequence = v.Sequence
		a.Hash = v.Hash
	}
	for index := 0; buffer.Len()+CompletionAllowance+140000 < MaxJournalBytes; index++ {
		r := Reservation{ID: fmt.Sprintf("old-%d", index), RequestHash: "hash", Scope: pending.Scope}
		appendEntry(LedgerEntry{Kind: "RESERVED", Reservation: r})
		appendEntry(LedgerEntry{Kind: "APPENDED", Reservation: r, Binding: &large})
	}
	// Use bounded final historical records to reach less than two allowances free.
	for index := 0; buffer.Len()+2*CompletionAllowance < MaxJournalBytes; index++ {
		r := Reservation{ID: fmt.Sprintf("tail-%d", index), RequestHash: "hash", Scope: pending.Scope}
		appendEntry(LedgerEntry{Kind: "RESERVED", Reservation: r})
		appendEntry(LedgerEntry{Kind: "APPENDED", Reservation: r, Binding: &large})
	}
	_ = entries
	if e = l.root.Replace("journal.jsonl", buffer.Bytes()); e != nil {
		t.Fatal(e)
	}
	anchor, _ := json.Marshal(a)
	if e = l.root.Replace("anchor.json", anchor); e != nil {
		t.Fatal(e)
	}
	before, e := l.LatestHead(ctx, "capacity")
	if e != nil {
		t.Fatal(e)
	}
	next := pending
	next.ID = "over-capacity"
	if e = l.ReserveOnce(ctx, next); e == nil {
		t.Fatal("accepted G0 without completion space")
	}
	after, e := l.LatestHead(ctx, "capacity")
	if e != nil || before != after {
		t.Fatal("capacity refusal damaged current head", e)
	}
	if e = l.AppendOnce(ctx, pending.ID, large); e != nil {
		t.Fatal("existing G0 could not finish", e)
	}
	if e = l.Ready(ctx); e != nil {
		t.Fatal(e)
	}
}
func TestLedgerLaggingAnchorRecoversOnlyContinuousTail(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	l, e := CreateLedger(dir, "install", "ledger", "anchor")
	if e != nil {
		t.Fatal(e)
	}
	old, e := l.root.Read("anchor.json")
	if e != nil {
		t.Fatal(e)
	}
	r := Reservation{ID: "task", RequestHash: "hash", Scope: []string{"t", "p", "w", "s", "c"}}
	if e = l.ReserveOnce(ctx, r); e != nil {
		t.Fatal(e)
	}
	if e = l.AppendOnce(ctx, r.ID, Binding{SourceEpoch: "epoch", SelectorHash: "hash"}); e != nil {
		t.Fatal(e)
	}
	if e = l.root.Replace("anchor.json", old); e != nil {
		t.Fatal(e)
	}
	h, e := l.LatestHead(ctx, "lagging")
	if e != nil || h.Sequence != 2 {
		t.Fatal("verified continuous tail not recovered", h, e)
	}
	raw, e := l.root.Read("journal.jsonl")
	if e != nil {
		t.Fatal(e)
	}
	split := bytes.Split(raw, []byte{'\n'})
	if e = l.root.Replace("journal.jsonl", append(split[1], '\n')); e != nil {
		t.Fatal(e)
	}
	if _, e = l.LatestHead(ctx, "gap"); e == nil {
		t.Fatal("gapped journal accepted")
	}
}

func TestMaximumGovernanceBindingFitsReservedCompletion(t *testing.T) {
	b := Binding{SelectorHash: strings.Repeat("f", 64), SourceEpoch: strings.Repeat("s", 64), Action: "CORRECT", TargetKind: "MEMORY_NODE", ReplacementID: strings.Repeat("r", 64), ReplacementHash: strings.Repeat("f", 64), Cutoff: ^uint64(0), Uses: []Use{PrivateResponse, PublicResponse, Relationship, Research}, Revisions: map[string]uint64{}}
	for i := 0; i < 1000; i++ {
		id := fmt.Sprintf("%04d%s", i, strings.Repeat("x", 60))
		b.Targets = append(b.Targets, id)
		b.Revisions[id] = ^uint64(0)
	}
	r := Reservation{ID: strings.Repeat("t", 64), RequestHash: strings.Repeat("f", 64), Scope: []string{strings.Repeat("s", 64), strings.Repeat("s", 64), strings.Repeat("s", 64), strings.Repeat("s", 64), strings.Repeat("s", 64)}}
	raw, e := json.Marshal(LedgerEntry{Sequence: ^uint64(0), PrevHash: strings.Repeat("f", 64), Hash: strings.Repeat("f", 64), Kind: "APPENDED", Reservation: r, Binding: &b})
	if e != nil || len(raw)+1 > CompletionAllowance {
		t.Fatal("maximum retained target/revision framing exceeds allowance", len(raw)+1, e)
	}
	t.Logf("maximum supported serialized completion incl framing/newline: %d bytes <= %d", len(raw)+1, CompletionAllowance)
}
