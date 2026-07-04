package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoS3ConversationCodeRemains is a grep-guard (spec component 3): the S3
// conversation restore/fork/upload machinery must be fully removed from the
// module, leaving only the compact handoff preamble. This file is itself
// excluded from the scan.
func TestNoS3ConversationCodeRemains(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	selfPath, err := filepath.Abs("guard_no_s3_test.go")
	if err != nil {
		t.Fatalf("resolve self path: %v", err)
	}

	banned := []string{
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
		abs, aerr := filepath.Abs(path)
		if aerr != nil {
			return aerr
		}
		if abs == selfPath {
			return nil
		}
		b, rerr := os.ReadFile(path) //nolint:gosec // path is from WalkDir over the repo's own tree, not user input
		if rerr != nil {
			return rerr
		}
		content := string(b)
		for _, needle := range banned {
			if strings.Contains(content, needle) {
				t.Errorf("%s: found banned S3/conversation-restore reference %q", path, needle)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo: %v", err)
	}
}
