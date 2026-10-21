package domain

// GenerationBinding is metadata owned by the generation-context module. Hashes
// are comparison values, never permissions or provider completion evidence.
type GenerationBinding struct {
	InputSourceID, TurnCommandHash                              string
	ActorID, DataSubjectID, TargetKind, TargetID, Audience, Use string
	ContextHash, RouteID, RequestHash, WorldHash                string
	// Component digests. A single total hash cannot distinguish "another task
	// added a memory this attempt never used" from "this attempt's own sources
	// changed"; these can, so an original result stays verifiable in a world
	// where several tasks advance at once.
	CoreHash, SkillsHash, MemoryGraphHash string
}

// SameOrigin reports whether two bindings describe the same request origin.
// The context, request and world digests are deliberately excluded: in a world
// where several tasks advance concurrently those legitimately move between an
// attempt and its later recovery, while the origin itself must not.
func (b GenerationBinding) SameOrigin(o GenerationBinding) bool {
	return b.InputSourceID == o.InputSourceID && b.TurnCommandHash == o.TurnCommandHash &&
		b.ActorID == o.ActorID && b.DataSubjectID == o.DataSubjectID &&
		b.TargetKind == o.TargetKind && b.TargetID == o.TargetID &&
		b.Audience == o.Audience && b.Use == o.Use && b.RouteID == o.RouteID
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
