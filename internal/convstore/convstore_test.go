package convstore

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/storage"
)

func TestProjectDirName(t *testing.T) {
	cases := map[string]string{
		"/workspace":     "-workspace",
		"/tmp/spike-ws":  "-tmp-spike-ws",
		"/home/agent/wd": "-home-agent-wd",
		"/a.b_c/d":       "-a-b-c-d",
	}
	for in, want := range cases {
		if got := ProjectDirName(in); got != want {
			t.Errorf("ProjectDirName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTranscriptDir(t *testing.T) {
	got := TranscriptDir("/home/agent", "/workspace")
	want := "/home/agent/.claude/projects/-workspace"
	if got != want {
		t.Errorf("TranscriptDir = %q, want %q", got, want)
	}
}

func TestSessionIDFromPath(t *testing.T) {
	const sid = "11111111-2222-3333-4444-555555555555"
	got := SessionIDFromPath("/home/agent/.claude/projects/-workspace/" + sid + ".jsonl")
	if got != sid {
		t.Errorf("SessionIDFromPath = %q, want %q", got, sid)
	}
}

func TestUpload_ReadsFileAndPuts(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "sess.jsonl")
	if err := os.WriteFile(path, []byte("line1\nline2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st := storage.NewMemStore()
	if err := Upload(ctx, st, "issue-1/conv.jsonl", path); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	ok, _ := st.Exists(ctx, "issue-1/conv.jsonl")
	if !ok {
		t.Fatal("uploaded object missing")
	}
	rc, _ := st.Get(ctx, "issue-1/conv.jsonl")
	b, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(b) != "line1\nline2\n" {
		t.Errorf("uploaded content = %q", b)
	}
}

func TestUpload_MissingFileErrors(t *testing.T) {
	if err := Upload(context.Background(), storage.NewMemStore(), "k", "/no/such/file.jsonl"); err == nil {
		t.Error("Upload of a missing file must error")
	}
}

func TestRestore_NoObjectStartsFresh(t *testing.T) {
	restored, err := Restore(context.Background(), storage.NewMemStore(), "absent", "sid", t.TempDir())
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if restored {
		t.Error("Restore must report false when the object is absent")
	}
}

func TestRestore_WritesSessionFileForResume(t *testing.T) {
	ctx := context.Background()
	st := storage.NewMemStore()
	const sid = "11111111-2222-3333-4444-555555555555"
	if err := st.Put(ctx, "issue-114/conv.jsonl", strings.NewReader(`{"sessionId":"x"}`)); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "projects", "-workspace")
	restored, err := Restore(ctx, st, "issue-114/conv.jsonl", sid, dir)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if !restored {
		t.Fatal("Restore must report true when the object exists")
	}
	got, err := os.ReadFile(filepath.Join(dir, sid+".jsonl"))
	if err != nil {
		t.Fatalf("restored transcript not written: %v", err)
	}
	if string(got) != `{"sessionId":"x"}` {
		t.Errorf("restored content = %q", got)
	}
}
