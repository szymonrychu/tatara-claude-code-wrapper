package turn

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
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

func TestRecord_HasNoConversationPointers(t *testing.T) {
	// Contract G.1. Both fields had ZERO writers for their whole life: declared,
	// JSON-tagged, documented against an operator replay contract that neither
	// side ever implemented. There is no cross-pod conversation to point at any
	// more - every pod's turn-0 is a fresh bundle (contract E.2).
	raw, err := json.Marshal(Record{ID: "t1", State: Complete})
	require.NoError(t, err)
	require.NotContains(t, string(raw), "sessionId")
	require.NotContains(t, string(raw), "conversationObjectKey")

	typ := reflect.TypeOf(Record{})
	for _, f := range []string{"SessionID", "ConversationObjectKey"} {
		_, ok := typ.FieldByName(f)
		require.False(t, ok, "turn.Record.%s must be gone", f)
	}
}

func TestRecord_KeepsPushedRepos(t *testing.T) {
	// Contract G.2: pushedRepos is RETAINED. Without it the operator cannot tell
	// "no diff" from "forgot to push" on a multi-repo Task, and the TTL synthetic
	// handoff note (G.7 step 4) is built from it.
	raw, err := json.Marshal(Record{ID: "t1", PushedRepos: []string{"tatara-cli"}})
	require.NoError(t, err)
	require.Contains(t, string(raw), "pushedRepos")
}
