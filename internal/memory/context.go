package memory

import (
	"sort"
	"strings"
	"time"
)

type Audience string

const (
	Private Audience = "PRIVATE"
	Public  Audience = "PUBLIC"
)

type Use string

const (
	PrivateResponse Use = "PRIVATE_RESPONSE"
	PublicResponse  Use = "PUBLIC_RESPONSE"
	Relationship    Use = "RELATIONSHIP"
	Research        Use = "RESEARCH_OPT_IN"
)

type EvidenceDomain string

const (
	WorldFact       EvidenceDomain = "WORLD_FACT"
	PlayerStatement EvidenceDomain = "PLAYER_STATEMENT"
	ActorBelief     EvidenceDomain = "ACTOR_BELIEF"
	Sandbox         EvidenceDomain = "SANDBOX"
)

type AcquisitionKind string

const (
	Initial   AcquisitionKind = "INITIAL"
	Witnessed AcquisitionKind = "WITNESSED"
	Told      AcquisitionKind = "TOLD"
	Inferred  AcquisitionKind = "INFERRED"
)

type Source struct {
	ID, Body      string
	Domain        EvidenceDomain
	Audience      Audience
	Uses          []Use
	Revision      uint64
	Blocked       bool
	Epoch         string
	Intake        uint64
	OccurredAt    time.Time
	EstimatedTime bool
}
type MemoryRecord struct {
	ID, Kind, Body, ActorID, TargetKind, TargetID, Corrects string
	Domain                                                  EvidenceDomain
	Audience                                                Audience
	Uses                                                    []Use
	Sources, Parents, MediaRefs                             []string
	Revision                                                uint64
	Blocked                                                 bool
	ExpiresAt                                               time.Time
}
type Grant struct {
	ActorID, SourceID string
	Uses              []Use
	Revoked           bool
	ExpiresAt         time.Time
}
type Acquisition struct {
	ActorID, SourceID string
	Kind              AcquisitionKind
	EvidenceID        string
}
type Aggregate struct {
	Sources                map[string]Source
	Nodes                  map[string]MemoryRecord
	Grants                 []Grant
	Acquisitions           []Acquisition
	Disabled               []Use
	Epoch                  string
	Intake, PolicyRevision uint64
}

func contains(uses []Use, wanted Use) bool {
	for _, u := range uses {
		if u == wanted {
			return true
		}
	}
	return false
}
func validAudience(source, output Audience) bool {
	return (source == Public || source == Private) && (output == Private || output == Public && source == Public)
}
func (a Aggregate) sourceAllowed(id, actor string, audience Audience, use Use, now time.Time) bool {
	s, ok := a.Sources[id]
	if !ok || s.Blocked || !validAudience(s.Audience, audience) || !contains(s.Uses, use) {
		return false
	}
	granted := false
	for _, g := range a.Grants {
		if g.ActorID == actor && g.SourceID == id && !g.Revoked && (g.ExpiresAt.IsZero() || now.Before(g.ExpiresAt)) && contains(g.Uses, use) {
			granted = true
		}
	}
	if !granted {
		return false
	}
	for _, v := range a.Acquisitions {
		if v.ActorID == actor && v.SourceID == id && v.EvidenceID != "" {
			return true
		}
	}
	return false
}

// Allowed computes the entire permitted set before relevance ranking. Every
// ancestor is rechecked; changing a descendant's label cannot erase ancestry.
func (a Aggregate) Allowed(actor string, audience Audience, use Use, now time.Time) []MemoryRecord {
	out := []MemoryRecord{}
	if contains(a.Disabled, use) {
		return out
	}
	// Tri-state traversal memoizes each node and source in this immutable
	// actor/audience/use snapshot. Shared DAGs are O(nodes + edges), not paths.
	state := map[string]uint8{}
	height := map[string]int{}
	sourceMemo := map[string]bool{}
	sourceSeen := map[string]bool{}
	var eligible func(string) bool
	eligible = func(id string) bool {
		if state[id] == 1 || state[id] == 3 {
			return false
		}
		if state[id] == 2 {
			return true
		}
		state[id] = 1
		deny := func() bool { state[id] = 3; return false }
		n, ok := a.Nodes[id]
		if !ok || n.ActorID != actor || n.Blocked || !validAudience(n.Audience, audience) || !contains(n.Uses, use) || !n.ExpiresAt.IsZero() && !now.Before(n.ExpiresAt) || len(n.Sources)+len(n.Parents) == 0 || len(n.Sources)+len(n.Parents) > 64 {
			return deny()
		}
		for _, source := range n.Sources {
			if !sourceSeen[source] {
				sourceSeen[source] = true
				sourceMemo[source] = a.sourceAllowed(source, actor, audience, use, now)
			}
			if !sourceMemo[source] {
				return deny()
			}
		}
		for _, parent := range n.Parents {
			if !eligible(parent) {
				return deny()
			}
			if height[parent]+1 > height[id] {
				height[id] = height[parent] + 1
			}
		}
		if height[id] > 32 {
			return deny()
		}
		state[id] = 2
		return true
	}
	if len(a.Nodes) > MaxNodes {
		return out
	}
	for id, n := range a.Nodes {
		if eligible(id) {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

type RecallQuery struct {
	Keywords []string
	From, To time.Time
	Limit    int
}

func (a Aggregate) Retrieve(actor string, audience Audience, use Use, q RecallQuery, now time.Time) ([]MemoryRecord, error) {
	if q.Limit < 1 || q.Limit > 100 || len(q.Keywords) > 16 || !q.From.IsZero() && !q.To.IsZero() && !q.From.Before(q.To) {
		return nil, ErrInvalid
	}
	for _, word := range q.Keywords {
		if len(word) == 0 || len(word) > 128 {
			return nil, ErrInvalid
		}
	}
	type match struct {
		record MemoryRecord
		score  int
		at     time.Time
	}
	ranked := []match{}
	for _, n := range a.Allowed(actor, audience, use, now) {
		score := 0
		for _, word := range q.Keywords {
			if strings.Contains(strings.ToLower(n.Body), strings.ToLower(word)) {
				score++
			}
		}
		if len(q.Keywords) > 0 && score == 0 {
			continue
		}
		var at time.Time
		for _, id := range n.Sources {
			v := a.Sources[id].OccurredAt
			if v.After(at) {
				at = v
			}
		}
		if !q.From.IsZero() && at.Before(q.From) || !q.To.IsZero() && !at.Before(q.To) {
			continue
		}
		ranked = append(ranked, match{n, score, at})
	}
	sort.Slice(ranked, func(i, j int) bool {
		a, b := ranked[i], ranked[j]
		if a.score != b.score {
			return a.score > b.score
		}
		if !a.at.Equal(b.at) {
			return a.at.After(b.at)
		}
		return a.record.ID < b.record.ID
	})
	if len(ranked) > q.Limit {
		ranked = ranked[:q.Limit]
	}
	out := []MemoryRecord{}
	for _, v := range ranked {
		out = append(out, v.record)
	}
	return out, nil
}
