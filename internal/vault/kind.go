// Package vault is the replay store: Tigris (S3) objects keyed by match id,
// their metadata, and the small control objects (done markers, run plans,
// Valve-fetch log) the re-parse machinery keeps next to them.
//
// It never talks to Valve: the Valve client lives in internal/valve so that
// the worker side (internal/reparse) cannot reach it.
package vault

import "strings"

// Kind says why a replay is kept.
type Kind string

const (
	KindStudent Kind = "student"
	KindPro     Kind = "pro"
	KindLadder  Kind = "ladder"
	KindCoach   Kind = "coach"
	KindAudit   Kind = "audit"
	KindCamp    Kind = "camp"
	// KindUnknown is what an object without a readable kind reads as: it is
	// kept (retention never touches it) and not re-parsed until a hint names a kind.
	KindUnknown Kind = "unknown"
)

var allKinds = []Kind{KindStudent, KindPro, KindLadder, KindCoach, KindAudit, KindCamp}

// ParseKind maps a hint to a Kind. An empty hint is a coach analysis; an
// unknown non-empty word is rejected.
func ParseKind(s string) (Kind, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return KindCoach, true
	}
	for _, k := range allKinds {
		if string(k) == s {
			return k, true
		}
	}
	return "", false
}

// Class decides conflicts when the same match is stored for two reasons:
// the higher class wins (student > audit,camp > pro,ladder > coach). Coach is
// the only deletable-by-default kind, so a pro or ladder hint must always beat
// it: on a tie the first kind stays, and a coach copy would otherwise shed the
// pro/ladder protection.
func (k Kind) Class() int {
	switch k {
	case KindStudent:
		return 4
	case KindAudit, KindCamp:
		return 3
	case KindPro, KindLadder:
		return 2
	case KindCoach:
		return 1
	}
	return 0
}

// Protected: a kind whose membership must survive a merge into a higher kind
// (everything but a plain coach analysis).
func (k Kind) Protected() bool { return k.Class() > KindCoach.Class() }

// ReparseOrder is the order a re-parse works through kinds: pages first
// (students, pros, medals), coach games after. Audit is never re-parsed.
func (k Kind) ReparseOrder() int {
	switch k {
	case KindStudent:
		return 0
	case KindPro:
		return 1
	case KindLadder:
		return 2
	case KindCoach:
		return 3
	case KindCamp:
		return 4
	}
	return 99
}

// Reparsed reports whether a re-parse delivers this kind.
func (k Kind) Reparsed() bool { return k != KindAudit && k.Class() > 0 }

// DeliveryMode is how the web side stores a delivered parse: ladder games go
// to Storage only (never a web_replays row), everything else through ingest.
func (k Kind) DeliveryMode() string {
	if k == KindLadder {
		return "storage"
	}
	return "ingest"
}

// HighestKind is the kind of the highest class among ks (the first on a tie);
// an empty list gives coach, the kind nobody protects.
func HighestKind(ks []Kind) Kind {
	best := KindCoach
	for i, k := range ks {
		if i == 0 || k.Class() > best.Class() {
			best = k
		}
	}
	return best
}
