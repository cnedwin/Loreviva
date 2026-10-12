package domain

// GenerationBinding is metadata owned by the generation-context module. Hashes
// are comparison values, never permissions or provider completion evidence.
type GenerationBinding struct {
	InputSourceID, TurnCommandHash                              string
	ActorID, DataSubjectID, TargetKind, TargetID, Audience, Use string
	ContextHash, RouteID, RequestHash, WorldHash                string
}

// GenerationSelection identifies the requested relationship, never permissions.
type GenerationSelection struct {
	ActorID, DataSubjectID, TargetKind, TargetID string
}

type TurnRouteAuthority struct{ ProjectID, ActorID, RouteID string }
type TurnInput struct {
	Scope                           Scope
	TaskID, ActorID, SourceID, Body string
}

// GenerationMaterial is transient. Persistent records retain only its binding.
type GenerationMaterial struct {
	Binding                   GenerationBinding
	Canonical                 []byte
	Connection, Model, Module string
	InputBound, OutputBound   uint64
}

type ResultSourceBinding struct {
	ActorID, SubjectID, Storage, BodyHash, Epoch string
	Nodes                                        map[string]string
}
