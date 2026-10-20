package vault

import (
	"sort"
	"strings"
	"time"
)

// Meta is the user metadata of a replay object.
type Meta struct {
	Kind      Kind
	Patch     string   // integer OpenDota patch id; "" = unknown
	Accounts  []string // steam32 as strings, sorted, unique
	Source    string
	Container string // bz2 | zst | raw
	StoredAt  string // RFC3339
	SHA256    string // set by local uploads
	Bad       bool   // downloaded cleanly but the parser failed
}

func digitsOnly(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// CleanPatch keeps an integer patch id and drops anything else (names like
// "7.41" are not ids).
func CleanPatch(s string) string {
	s = strings.TrimSpace(s)
	if digitsOnly(s) && len(s) <= 6 {
		return s
	}
	return ""
}

// CleanAccounts keeps digit-only steam32 strings, sorted and unique.
func CleanAccounts(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, a := range in {
		a = strings.TrimSpace(a)
		if len(a) > 20 || !digitsOnly(a) || seen[a] {
			continue
		}
		seen[a] = true
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// SplitAccounts splits "1,2;3" style lists.
func SplitAccounts(s string) []string {
	return CleanAccounts(strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';' || r == '|' || r == ' '
	}))
}

// ToS3 renders the metadata as lowercase S3 user-metadata (no prefix).
func (m Meta) ToS3() map[string]string {
	out := map[string]string{}
	if m.Kind != "" {
		out["kind"] = string(m.Kind)
	}
	if p := CleanPatch(m.Patch); p != "" {
		out["patch"] = p
	}
	if len(m.Accounts) > 0 {
		out["accounts"] = strings.Join(CleanAccounts(m.Accounts), ",")
	}
	if m.Source != "" {
		out["source"] = m.Source
	}
	if m.Container != "" {
		out["container"] = m.Container
	}
	if m.StoredAt != "" {
		out["stored-at"] = m.StoredAt
	}
	if m.SHA256 != "" {
		out["sha256"] = m.SHA256
	}
	if m.Bad {
		out["bad"] = "1"
	}
	return out
}

// MetaFromS3 is the inverse of ToS3; an unknown or missing kind reads as coach.
func MetaFromS3(in map[string]string) Meta {
	get := func(k string) string {
		for kk, v := range in {
			if strings.EqualFold(kk, k) {
				return v
			}
		}
		return ""
	}
	k, ok := ParseKind(get("kind"))
	if !ok {
		k = KindCoach
	}
	return Meta{
		Kind:      k,
		Patch:     CleanPatch(get("patch")),
		Accounts:  SplitAccounts(get("accounts")),
		Source:    get("source"),
		Container: get("container"),
		StoredAt:  get("stored-at"),
		SHA256:    get("sha256"),
		Bad:       get("bad") == "1",
	}
}

// Merge folds want into have: the higher class wins (within a class the
// first-written kind stays), accounts are unioned, an empty patch is filled,
// bad is sticky. changed says whether the stored metadata must be rewritten.
func Merge(have, want Meta) (Meta, bool) {
	out := have
	if want.Kind.Class() > have.Kind.Class() {
		out.Kind = want.Kind
	}
	out.Accounts = CleanAccounts(append(append([]string{}, have.Accounts...), want.Accounts...))
	if out.Patch == "" {
		out.Patch = CleanPatch(want.Patch)
	}
	if out.Source == "" {
		out.Source = want.Source
	}
	if out.Container == "" {
		out.Container = want.Container
	}
	if out.StoredAt == "" {
		out.StoredAt = want.StoredAt
	}
	if out.SHA256 == "" {
		out.SHA256 = want.SHA256
	}
	out.Bad = have.Bad || want.Bad
	return out, !equalMeta(have, out)
}

func equalMeta(a, b Meta) bool {
	am, bm := a.ToS3(), b.ToS3()
	if len(am) != len(bm) {
		return false
	}
	for k, v := range am {
		if bm[k] != v {
			return false
		}
	}
	return true
}

// Now is the clock; tests replace it.
var Now = func() time.Time { return time.Now().UTC() }
