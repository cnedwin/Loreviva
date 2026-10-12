package domain

import "encoding/json"

// These are closed internal authoring identities, never runtime actor scopes.
type DraftCandidateSelection struct {
	DraftID, ObjectID, ModuleID string
	Revision                    uint64
	Fields                      []string
}
type DraftCandidatePrompt struct {
	Selection                        DraftCandidateSelection
	DraftHash, RouteHash, SourceHash string
	Values                           map[string]json.RawMessage
	Settings                         json.RawMessage
}
type DraftCandidateMaterial struct {
	Selection                                    DraftCandidateSelection
	DraftHash, RouteHash, RequestHash, ProjectID string
	Connection, Model                            string
	Canonical                                    []byte
	InputBound, OutputBound                      uint64
	SourceHash                                   string
	Sources                                      []CandidateSource
}

// Frozen by the owning generation transaction. These references and hashes
// bind the actual selected source graph, but never grant current use rights.
type CandidateSource struct {
	ReleaseID, ObjectID, Kind, Version, Hash string
	AssetID, AssetHash                       string
}
