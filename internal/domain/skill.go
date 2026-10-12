package domain

// TurnSkill is server-resolved metadata. It conveys no permission or body.
// Empty Skills are omitted from the original canonical preimage.
type TurnSkill struct {
	BindingID, DefinitionHash, RoleHash, SourceRelease, Capability, InputSchema, OutputSchema string
	MaxDepth                                                                                  uint64
	DurationMS                                                                                uint64
	Order                                                                                     int64
}

// Transient selected Skill content travels only in the already-governed sole
// generation material. Invocation records contain TurnSkill metadata only.
type TurnSkillInput struct {
	TurnSkill
	Description string
	Examples    []SkillExample
}
type SkillExample struct{ Input, Expected string }
