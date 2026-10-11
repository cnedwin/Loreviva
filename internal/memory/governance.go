package memory

import (
	"loreviva.local/loreviva/internal/domain"
	"sort"
	"time"
	"unicode/utf8"
)

type Selector struct {
	Kind, TargetKind string
	IDs              []string
	Expected         map[string]uint64
	From, To         time.Time
}

// Block fixes the committed source cutoff before adding irreversible blocking.
// Node selection never expands upstream to an independent original source.
func (a *Aggregate) Block(sel Selector, action string, uses []Use, replacementID, replacement string) (Binding, error) {
	b := Binding{SelectorHash: digest(sel), SourceEpoch: a.Epoch, Cutoff: a.Intake, Action: action, TargetKind: sel.TargetKind, Revisions: sel.Expected}
	if action != "DELETE" && action != "REVOKE" && action != "CORRECT" {
		return b, ErrInvalid
	}
	if action != "CORRECT" && (replacement != "" || replacementID != "") {
		return b, ErrInvalid
	}
	if uses != nil && !validUses(uses) {
		return b, ErrInvalid
	}
	selected := map[string]bool{}
	sources := map[string]bool{}
	switch sel.Kind {
	case "IDS":
		if len(sel.IDs) == 0 || len(sel.IDs) > 1000 || len(sel.IDs) != len(sel.Expected) {
			return b, ErrInvalid
		}
		for _, id := range sel.IDs {
			if domain.ValidateID(id) != nil || selected[id] {
				return b, ErrInvalid
			}
			selected[id] = true
			rev, ok := sel.Expected[id]
			if !ok {
				return b, ErrInvalid
			}
			if sel.TargetKind == "MEMORY_NODE" {
				n, ok := a.Nodes[id]
				if !ok || n.Revision != rev {
					return b, ErrInvalid
				}
			} else if sel.TargetKind == "SOURCE" {
				s, ok := a.Sources[id]
				if !ok || s.Revision != rev {
					return b, ErrInvalid
				}
				sources[id] = true
			} else {
				return b, ErrInvalid
			}
		}
	case "TIME_RANGE", "ALL_EXISTING":
		if len(sel.IDs) > 0 || len(sel.Expected) > 0 || sel.TargetKind != "SOURCE" {
			return b, ErrInvalid
		}
		if sel.Kind == "TIME_RANGE" && (sel.From.IsZero() || !sel.From.Before(sel.To) || sel.From.Location() != time.UTC || sel.To.Location() != time.UTC) {
			return b, ErrInvalid
		}
		for id, s := range a.Sources {
			if s.Intake <= a.Intake && (sel.Kind == "ALL_EXISTING" || !s.OccurredAt.Before(sel.From) && s.OccurredAt.Before(sel.To)) {
				sources[id] = true
				selected[id] = true
			}
		}
	default:
		return b, ErrInvalid
	}
	if action == "CORRECT" && (sel.Kind != "IDS" || sel.TargetKind != "MEMORY_NODE" || len(selected) != 1 || replacement == "" || utf8.RuneCountInString(replacement) > 16000 || domain.ValidateID(replacementID) != nil) {
		return b, ErrInvalid
	}
	if action == "CORRECT" {
		if _, ok := a.Nodes[replacementID]; ok {
			return b, ErrInvalid
		}
		if a.Nodes[sel.IDs[0]].Domain == WorldFact {
			return b, ErrInvalid
		}
		if len(a.Nodes) >= MaxNodes {
			return b, ErrInvalid
		}
	}
	for id := range selected {
		b.Targets = append(b.Targets, id)
	}
	sort.Strings(b.Targets)
	b.Uses = uses
	blocked := map[string]bool{}
	if sel.TargetKind == "MEMORY_NODE" {
		for id := range selected {
			blocked[id] = true
		}
	}
	for id, n := range a.Nodes {
		for _, source := range n.Sources {
			if sources[source] {
				blocked[id] = true
			}
		}
	}
	for changed := true; changed; {
		changed = false
		for id, n := range a.Nodes {
			if blocked[id] {
				continue
			}
			for _, parent := range n.Parents {
				if blocked[parent] {
					blocked[id] = true
					changed = true
					break
				}
			}
		}
	}
	for id := range sources {
		s := a.Sources[id]
		if action == "REVOKE" && len(uses) > 0 {
			s.Uses = removeUses(s.Uses, uses)
		} else {
			s.Blocked = true
		}
		s.Revision++
		a.Sources[id] = s
	}
	for id := range blocked {
		n := a.Nodes[id]
		if action == "REVOKE" && len(uses) > 0 {
			n.Uses = removeUses(n.Uses, uses)
			if len(n.Uses) == 0 {
				n.Blocked = true
			}
		} else {
			n.Blocked = true
		}
		n.Revision++
		a.Nodes[id] = n
	}
	if action == "CORRECT" {
		old := a.Nodes[sel.IDs[0]]
		old.ID = replacementID
		old.Body = replacement
		old.Blocked = false
		old.Revision = 1
		old.Corrects = sel.IDs[0]
		a.Nodes[replacementID] = old
		b.ReplacementID = replacementID
		b.ReplacementHash = digest(replacement)
	}
	return b, nil
}
func removeUses(have, remove []Use) []Use {
	out := []Use{}
	for _, u := range have {
		if !contains(remove, u) {
			out = append(out, u)
		}
	}
	return out
}
func (a *Aggregate) Clean() {
	for id, n := range a.Nodes {
		if n.Blocked {
			n.Body = ""
			n.MediaRefs = nil
			a.Nodes[id] = n
		}
	}
	for id, s := range a.Sources {
		if s.Blocked {
			s.Body = ""
			a.Sources[id] = s
		}
	}
}
func (a *Aggregate) AddDerived(n MemoryRecord, now time.Time) error {
	if domain.ValidateID(n.ID) != nil || n.Domain != ActorBelief || n.Revision != 1 || len(a.Nodes) >= MaxNodes || len(n.Body) > 8192 || len(n.Parents) == 0 || len(n.Parents)+len(n.Sources) > MaxReferences || !validUses(n.Uses) || len(n.MediaRefs) != 0 {
		return ErrInvalid
	}
	if _, ok := a.Nodes[n.ID]; ok {
		return ErrInvalid
	}
	for _, u := range n.Uses {
		allowed := map[string]bool{}
		for _, v := range a.Allowed(n.ActorID, n.Audience, u, now) {
			allowed[v.ID] = true
		}
		for _, parent := range n.Parents {
			if !allowed[parent] {
				return ErrInvalid
			}
		}
		for _, source := range n.Sources {
			if !a.sourceAllowed(source, n.ActorID, n.Audience, u, now) {
				return ErrInvalid
			}
		}
	}
	// Preserve the complete stable source closure as well as parents.
	set := map[string]bool{}
	for _, source := range n.Sources {
		set[source] = true
	}
	visited := map[string]bool{}
	var visit func(string, int) error
	visit = func(id string, depth int) error {
		if depth > 32 {
			return ErrInvalid
		}
		if visited[id] {
			return nil
		}
		visited[id] = true
		p, ok := a.Nodes[id]
		if !ok {
			return ErrInvalid
		}
		for _, source := range p.Sources {
			set[source] = true
		}
		for _, parent := range p.Parents {
			if e := visit(parent, depth+1); e != nil {
				return e
			}
		}
		return nil
	}
	for _, id := range n.Parents {
		if e := visit(id, 1); e != nil {
			return e
		}
	}
	if len(set)+len(n.Parents) > MaxReferences {
		return ErrInvalid
	}
	n.Sources = nil
	for source := range set {
		n.Sources = append(n.Sources, source)
	}
	sort.Strings(n.Sources)
	a.Nodes[n.ID] = n
	for _, u := range n.Uses {
		valid := false
		for _, v := range a.Allowed(n.ActorID, n.Audience, u, now) {
			if v.ID == n.ID {
				valid = true
			}
		}
		if !valid {
			delete(a.Nodes, n.ID)
			return ErrInvalid
		}
	}
	delete(a.Nodes, n.ID)
	if len(a.Acquisitions)+len(n.Sources) > MaxAcquisitions {
		return ErrInvalid
	}
	for _, source := range n.Sources {
		a.Acquisitions = append(a.Acquisitions, Acquisition{ActorID: n.ActorID, SourceID: source, Kind: Inferred, EvidenceID: n.ID})
	}
	a.Nodes[n.ID] = n
	return nil
}
