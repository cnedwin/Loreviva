package domain

type CandidateResource struct{ Kind, ID, Hash string }
type CandidateApproval struct {
	Metadata  MockResultMetadata
	Versions  Versions
	ResultID  string
	Resources []CandidateResource
}

// CandidateCommit contains safe identities only. Candidate text remains in the
// governed result body; no journal, receipt or outbox copies that text.
type CandidateCommit struct {
	OriginalDependency   *MockResultMetadata `json:",omitempty"`
	NarrativeGeneration  uint64
	Metadata             MockResultMetadata
	ResultID, ReceiptKey string
	Versions             Versions
	EventIDs, MemoryIDs  []string
}
