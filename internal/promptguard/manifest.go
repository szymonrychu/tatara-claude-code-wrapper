package promptguard

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
)

// toolManifest is the shape tatara-cli's `tatara tool-manifest` emits and its
// release workflow uploads as a public, tokenless per-tag release asset. Only
// the names are read here; the enums are the sibling repo's concern.
//
// NOTE the bound this buys, because it is smaller than it looks: the manifest
// carries names and enums and NO profile data. It proves a tool name exists in
// the pinned cli. It does NOT prove any profile grants it. The live oracle in
// the Dockerfile test-guard stage (liveToolNames(t, "refine")) remains the
// stronger check and must stay.
type toolManifest struct {
	Tools []struct {
		Name string `json:"name"`
	} `json:"tools"`
}

// ManifestToolNames returns the sorted, de-duplicated tool names in a
// tool-manifest.json document.
//
// It fails closed on every shape that could be read as "nothing is missing": a
// body that is not the manifest, a manifest with no tools, or an entry with no
// name. That is Literals's contract, not the warn-and-return-0 one
// tatara-agent-skills' validate_tool_calls.py has - a validator that treats an
// unreadable oracle as a pass is how a dead name survives review.
func ManifestToolNames(r io.Reader) ([]string, error) {
	var m toolManifest
	if err := json.NewDecoder(r).Decode(&m); err != nil {
		return nil, fmt.Errorf("promptguard: decode tool manifest: %w", err)
	}
	seen := map[string]bool{}
	for i, t := range m.Tools {
		if t.Name == "" {
			return nil, fmt.Errorf("promptguard: tool manifest entry %d has no name; the manifest is not the shape this guard reads and would fail open", i)
		}
		seen[t.Name] = true
	}
	if len(seen) == 0 {
		return nil, fmt.Errorf("promptguard: tool manifest advertises no tools; an empty oracle passes every name and would fail open")
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}
