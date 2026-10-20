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
	// Bad: downloaded cleanly but the parser rejected it. BadVer is the parser
	// version that said so ("" for a flag without one); the re-parse skips the
	// replay only for that version, so a fixed release tries it again.
	Bad    bool
	BadVer string
	// Corrupt: the stored bytes do not decode. The next clean download of the
	// same match replaces the object (PutNew), and a backfill re-fetches it.
	Corrupt bool
	// Also lists the protected kinds (pro, ladder, ...) this object was also
	// stored as before a higher kind absorbed them, so removing the higher
	// kind's claim (purge-student) does not forget them.
	Also []Kind
}

// BadFor: flagged bad by this parser version.
func (m Meta) BadFor(ver string) bool { return m.Bad && m.BadVer != "" && m.BadVer == ver }

func cleanVer(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 32 {
		return ""
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '.' || c == '-' || c == '_') {
			return ""
		}
	}
	return s
}

func sortedKinds(in []Kind) []Kind {
	seen := map[Kind]bool{}
	var out []Kind
	for _, k := range in {
		if k.Protected() && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
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
	if m.Kind != "" && m.Kind != KindUnknown {
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
		if v := cleanVer(m.BadVer); v != "" && v != "1" {
			out["bad"] = v
		}
	}
	if m.Corrupt {
		out["corrupt"] = "1"
	}
	if also := sortedKinds(m.Also); len(also) > 0 {
		names := make([]string, len(also))
		for i, k := range also {
			names[i] = string(k)
		}
		out["also"] = strings.Join(names, ",")
	}
	return out
}

// MetaFromS3 is the inverse of ToS3. A missing or unknown kind reads as
// KindUnknown, which retention keeps: guessing "coach" there would make an
// object nobody classified deletable.
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
	if !ok || strings.TrimSpace(get("kind")) == "" {
		k = KindUnknown
	}
	bad := strings.TrimSpace(get("bad"))
	isBad := bad != "" && bad != "0"
	badVer := ""
	if isBad && bad != "1" {
		badVer = cleanVer(bad)
	}
	var also []Kind
	for _, name := range strings.Split(get("also"), ",") {
		if ak, ok := ParseKind(name); ok && strings.TrimSpace(name) != "" {
			also = append(also, ak)
		}
	}
	return Meta{
		Kind:      k,
		Patch:     CleanPatch(get("patch")),
		Accounts:  SplitAccounts(get("accounts")),
		Source:    get("source"),
		Container: get("container"),
		StoredAt:  get("stored-at"),
		SHA256:    get("sha256"),
		Bad:       isBad,
		BadVer:    badVer,
		Corrupt:   get("corrupt") == "1",
		Also:      sortedKinds(also),
	}
}

// Merge folds want into have: the higher class wins (within a class the
// first-written kind stays), accounts are unioned, an empty patch is filled,
// a bad flag is taken over (a merge never clears it: ClearBad does). The kind
// that loses is remembered in Also when it is a protected one. changed says
// whether the stored metadata must be rewritten.
func Merge(have, want Meta) (Meta, bool) {
	out := have
	if want.Kind.Class() > have.Kind.Class() {
		out.Kind = want.Kind
	}
	also := append(append([]Kind{}, have.Also...), want.Also...)
	also = append(also, have.Kind, want.Kind)
	out.Also = nil
	for _, k := range sortedKinds(also) {
		if k != out.Kind {
			out.Also = append(out.Also, k)
		}
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
	if want.Bad {
		out.Bad = true
		if want.BadVer != "" || !have.Bad {
			out.BadVer = want.BadVer
		}
	}
	out.Corrupt = have.Corrupt || want.Corrupt
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
