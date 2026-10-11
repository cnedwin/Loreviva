package memory

import (
	"testing"
	"time"
)

func TestCorrectionWinsWithoutErasingValidHistory(t *testing.T) {
	a := fixture()
	a.Nodes["independent"] = MemoryRecord{ID: "independent", Body: "independent valid history", Kind: "PROMISE", Domain: PlayerStatement, Audience: Private, Uses: []Use{PrivateResponse}, Sources: []string{"dm"}, Revision: 1}
	b, e := a.Block(Selector{Kind: "IDS", TargetKind: "MEMORY_NODE", IDs: []string{"preference"}, Expected: map[string]uint64{"preference": 1}}, "CORRECT", nil, "corrected", "updated preference")
	if e != nil {
		t.Fatal(e)
	}
	if len(b.Targets) != 1 {
		t.Fatal(b)
	}
	if !a.Nodes["preference"].Blocked || a.Nodes["independent"].Blocked {
		t.Fatal("correction erased independent valid history")
	}
	if a.Nodes["corrected"].Body != "updated preference" {
		t.Fatal("replacement missing")
	}
}
func TestDeleteBlocksLateProposalAndClearsDerivedMedia(t *testing.T) {
	a := fixture()
	a.Nodes["media"] = MemoryRecord{ID: "media", Kind: "MEDIA", Domain: ActorBelief, Body: "private media reference", Parents: []string{"preference"}, Audience: Private, Uses: []Use{PrivateResponse}, Revision: 1}
	if _, e := a.Block(Selector{Kind: "IDS", TargetKind: "MEMORY_NODE", IDs: []string{"preference"}, Expected: map[string]uint64{"preference": 1}}, "DELETE", nil, "", ""); e != nil {
		t.Fatal(e)
	}
	if e := a.AddDerived(MemoryRecord{ID: "late", ActorID: "alex", Kind: "GOAL", Domain: ActorBelief, Parents: []string{"media"}, Uses: []Use{PrivateResponse}, Audience: Private, Revision: 1}, time.Now()); e == nil {
		t.Fatal("late descendant admitted")
	}
	a.Clean()
	if a.Nodes["media"].Body != "" || a.Nodes["preference"].Body != "" {
		t.Fatal("blocked derived body retained")
	}
	if a.Sources["dm"].Blocked {
		t.Fatal("node deletion expanded upstream")
	}
}

func TestUsePolicyDisableAndEnableCannotReviveOldDerivative(t *testing.T) {
	a := fixture()
	a.PolicyRevision = 1
	a.Nodes["derived"] = MemoryRecord{ID: "derived", ActorID: "alex", Kind: "GOAL", Domain: ActorBelief, Body: "private goal", Parents: []string{"preference"}, Audience: Private, Uses: []Use{PrivateResponse}, Revision: 1}
	if e := a.SetUsePolicy(1, "DISABLE", []Use{PrivateResponse}); e != nil {
		t.Fatal(e)
	}
	if len(a.Allowed("alex", Private, PrivateResponse, time.Now())) != 0 {
		t.Fatal("disabled use recalled")
	}
	if e := a.SetUsePolicy(2, "ENABLE", []Use{PrivateResponse}); e != nil {
		t.Fatal(e)
	}
	if !a.Nodes["derived"].Blocked {
		t.Fatal("ENABLE revived old derivative")
	}
	if len(a.Allowed("alex", Private, PrivateResponse, time.Now())) != 1 {
		t.Fatal("source permission did not survive policy enable")
	}
}

func TestSourceTimeRangeCutoffAndRestoredEpochRemainExact(t *testing.T) {
	a := fixture()
	a.Epoch = "restored"
	a.Intake = 2
	s := a.Sources["dm"]
	s.Epoch = "historical"
	s.Intake = 1
	s.OccurredAt = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	a.Sources["dm"] = s
	independent := s
	independent.ID = "independent"
	independent.Intake = 2
	independent.OccurredAt = s.OccurredAt.Add(time.Hour)
	a.Sources[independent.ID] = independent
	b, e := a.Block(Selector{Kind: "TIME_RANGE", TargetKind: "SOURCE", From: s.OccurredAt, To: s.OccurredAt.Add(time.Minute)}, "DELETE", nil, "", "")
	if e != nil || len(b.Targets) != 1 || b.Targets[0] != "dm" || b.Cutoff != 2 || a.Sources["independent"].Blocked {
		t.Fatal("time/source cutoff differs", b, e)
	}
	a.Sources["later"] = Source{ID: "later", Epoch: "restored", Intake: 3, Revision: 1}
	a.Intake = 3
	replayBlock(&a, b)
	if a.Sources["later"].Blocked {
		t.Fatal("resolved selector acquired a later source")
	}
	all, e := a.Block(Selector{Kind: "ALL_EXISTING", TargetKind: "SOURCE"}, "DELETE", nil, "", "")
	if e != nil || len(all.Targets) != 3 {
		t.Fatal("historical epoch excluded from ALL_EXISTING", all, e)
	}
}
