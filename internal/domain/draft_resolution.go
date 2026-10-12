package domain

import "loreviva.local/loreviva/internal/generated"

// Internal owning view. Draft bytes are never serialized by the public resolver.
type DraftGeneration struct {
	Draft                                           generated.ContentDraft `json:"-"`
	ObjectID, ModuleID, OwnerKind, RouteHash, State string
	Settings                                        generated.GenerationSettings
	Fields                                          []string
}
