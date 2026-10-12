package domain

// A live owning lease over the independent provider journal. Its public shape
// contains no body reference or permission; the registered provider validates
// its own concrete evidence at each SQL boundary.
type SettlementContinuityEvidence interface{ Close() }
