package memory

func (a *Aggregate) SetUsePolicy(expected uint64, action string, uses []Use) error {
	if expected != a.PolicyRevision || expected == 0 || expected == ^uint64(0) || !validUses(uses) || (action != "DISABLE" && action != "ENABLE") {
		return ErrInvalid
	}
	if action == "DISABLE" {
		for _, u := range uses {
			if !contains(a.Disabled, u) {
				a.Disabled = append(a.Disabled, u)
			}
		}
		for id, n := range a.Nodes {
			if len(n.Parents) == 0 {
				continue
			}
			for _, u := range uses {
				if contains(n.Uses, u) {
					n.Blocked = true
					n.Revision++
					a.Nodes[id] = n
					break
				}
			}
		}
	} else {
		a.Disabled = removeUses(a.Disabled, uses)
	}
	a.PolicyRevision++
	return nil
}
