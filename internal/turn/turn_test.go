package turn

import (
	"encoding/json"
	"testing"
)

func TestRecord_InternalIssuesJSONRoundTrip(t *testing.T) {
	rec := Record{
		ID:    "turn-1",
		State: Complete,
		InternalIssues: []InternalIssueReport{
			{
				Category:      "tool_error",
				Severity:      "error",
				Description:   "the tool blew up",
				OffendingTool: "Bash",
				ResourceID:    "res-1",
			},
		},
	}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}
	issues, ok := got["internalIssues"].([]any)
	if !ok || len(issues) != 1 {
		t.Fatalf("internalIssues = %v, want a 1-element array", got["internalIssues"])
	}
	issue := issues[0].(map[string]any)
	for field, want := range map[string]string{
		"category":       "tool_error",
		"severity":       "error",
		"description":    "the tool blew up",
		"offending_tool": "Bash",
		"resource_id":    "res-1",
	} {
		if issue[field] != want {
			t.Errorf("issue[%q] = %v, want %q", field, issue[field], want)
		}
	}

	var rt Record
	if err := json.Unmarshal(b, &rt); err != nil {
		t.Fatalf("unmarshal to Record: %v", err)
	}
	if len(rt.InternalIssues) != 1 || rt.InternalIssues[0] != rec.InternalIssues[0] {
		t.Errorf("round-tripped InternalIssues = %+v, want %+v", rt.InternalIssues, rec.InternalIssues)
	}
}

func TestRecord_InternalIssuesOmittedWhenEmpty(t *testing.T) {
	rec := Record{ID: "turn-2", State: Complete}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := got["internalIssues"]; present {
		t.Errorf("internalIssues must be omitted when empty, got: %v", got["internalIssues"])
	}
}
