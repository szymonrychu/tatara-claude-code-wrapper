package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoReapedAPIsRemain is the regression guard for two reaps: the 2026-07-04
// S3 conversation-restore removal, and the 2026-07-12 task-centric removal of
// cross-pod continuity (/v1/interject, the handoff preamble, the conversation
// pointers) and the dead OTEL path. Every entry below is an identifier that was
// deliberately deleted and must not come back by copy-paste, by a revert, or by
// a well-meaning "restore the resume path" change.
//
// It does NOT ban the intra-pod crash-recovery machinery - convstore.TranscriptDir,
// claudeArgs, --continue, shouldResume, relaunch, TurnResumes - which is a
// DIFFERENT mechanism (one pod, one crashed claude process) and is load-bearing.
//
// The walk skips every file ending in _test.go (which subsumes skipping this
// file itself): the ban protects PRODUCTION code. A Go symbol reintroduced in a
// test cannot compile unless the production symbol also came back, so the
// compiler already guards those; and a string literal in a test is how absence
// of the reaped surface is ASSERTED (e.g. a test that checks a route 404s must
// name that route).
func TestNoReapedAPIsRemain(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}

	banned := []string{
		// --- 2026-07-04: S3 conversation restore ---
		"internal/storage",
		"convstore.Restore",
		"convstore.Fork(",
		"convstore.Upload(",
		"convstore.SessionIDFromPath",
		"convstore.ParseSessionID",
		"ConversationSessionID",
		"ConversationForkFromKey",
		"ConversationOpsTotal",
		"ResumeSessionID",
		"S3Config",
		"S3Bucket",
		"S3Endpoint",
		"S3Region",
		"S3KeyPrefix",
		"S3ForcePathStyle",
		"S3AccessKeyID",
		"S3SecretKey",
		"storage.Store",
		"storage.New(",
		"storage.Config",
		"storage.NewMemStore",

		// --- 2026-07-12: cross-pod continuity (contract G.1, G.5, G.9) ---
		"handoffPreambleFmt",
		"Continuation key",
		"HandoffKey",
		"handoffSent",
		"CONVERSATION_OBJECT_KEY",
		"CONVERSATION_SESSION_ID",
		"ConversationObjectKey",
		"postInterject",
		"ErrNotBusy",
		"Interjections",
		"ccw_interjections_total",
		"/v1/interject",
		"TATARA_CHAT_URL",
		"get_handoff",
		"write_handoff",

		// --- 2026-07-12: the dead OTEL path ---
		"OtelEnabled",
		"OtelEndpoint",
		"OTEL_ENABLED",
		"OTEL_EXPORTER_OTLP_ENDPOINT",
	}

	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, rerr := os.ReadFile(path) //nolint:gosec // path is from WalkDir over the repo's own tree, not user input
		if rerr != nil {
			return rerr
		}
		content := string(b)
		for _, needle := range banned {
			if strings.Contains(content, needle) {
				t.Errorf("%s: found banned reaped-API reference %q", path, needle)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo: %v", err)
	}
}
