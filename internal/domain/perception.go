package domain

// WorldPerception contains only current actor-visible runtime facts and opaque
// source identities. Authoring documents and private notes never enter it.
type PerceivedActor struct{ ID, Slot string }
type PerceivedItem struct {
	ID, CustodianID, HolderID, State string
	Capabilities                     []string
}
type WorldPerception struct {
	ActorID, CreationID, Scene string
	Versions                   Versions
	Actors                     []PerceivedActor
	Items                      []PerceivedItem
	Eligibility                map[string]string
	Resources                  []CandidateResource
}
