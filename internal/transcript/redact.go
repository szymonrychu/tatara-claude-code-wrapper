package transcript

import (
	"sort"
	"strings"
)

// Redactor replaces known secret values in strings with [REDACTED:<name>].
type Redactor struct {
	// ordered longest-first to handle secrets that contain shorter secrets
	pairs []secretPair
}

type secretPair struct {
	name  string
	value string
}

// NewRedactor builds a Redactor from a name->value map. Values shorter than
// 8 characters are skipped to avoid scrubbing trivial strings.
func NewRedactor(values map[string]string) *Redactor {
	var pairs []secretPair
	for name, val := range values {
		if len(val) < 8 {
			continue
		}
		pairs = append(pairs, secretPair{name: name, value: val})
	}
	// Sort longest value first so a secret containing another shorter one is
	// replaced in full, not partially.
	sort.Slice(pairs, func(i, j int) bool {
		return len(pairs[i].value) > len(pairs[j].value)
	})
	return &Redactor{pairs: pairs}
}

// Scrub replaces each registered secret value in s with [REDACTED:<name>].
func (r *Redactor) Scrub(s string) string {
	for _, p := range r.pairs {
		s = strings.ReplaceAll(s, p.value, "[REDACTED:"+p.name+"]")
	}
	return s
}
