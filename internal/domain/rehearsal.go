package domain

import "loreviva.local/loreviva/internal/generated"

// Immutable safe metadata. Original input text lives only in its governed
// rehearsal input source, never in this binding, an accounting row or the J.
type RehearsalInputBinding struct{ ActorSlot, Hash string }
type RehearsalBinding struct {
	DraftID, ProjectID, DraftHash, SourceHash, RouteHash string
	Revision                                             uint64
	Settings                                             generated.GenerationSettings
	Sources                                              []CandidateSource
	Inputs                                               []RehearsalInputBinding
	Assertions                                           []string
}

// Transient qualified material returned only from a fixed owning Store UoW.
// It is never a serialized permission and is not retained outside governed input.
type RehearsalBasis struct {
	RootTaskID, ActorID string
	Binding             RehearsalBinding
	Request             generated.RehearsalRequest
}

// Program findings carry no permission or private source text.
type RehearsalAssertion struct {
	Kind  string `json:"kind"`
	State string `json:"state"`
}
