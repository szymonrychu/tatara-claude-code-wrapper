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

func TestParseSessionID(t *testing.T) {
	jsonl := `{"type":"mode","sessionId":"sid-parent","mode":"x"}` + "\n" +
		`{"type":"user","sessionId":"sid-parent","message":{}}` + "\n"
	if got := ParseSessionID(strings.NewReader(jsonl)); got != "sid-parent" {
		t.Errorf("ParseSessionID = %q, want sid-parent", got)
	}
	if got := ParseSessionID(strings.NewReader("not json\n")); got != "" {
		t.Errorf("ParseSessionID of junk = %q, want empty", got)
	}
}

func TestFork_CopiesParentAndWritesSessionFile(t *testing.T) {
	ctx := context.Background()
	st := storage.NewMemStore()
	const parentSID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	parentContent := `{"type":"mode","sessionId":"` + parentSID + `"}` + "\n" +
		`{"type":"user","sessionId":"` + parentSID + `","message":{}}` + "\n"
	if err := st.Put(ctx, "tatara/task-brainstorm.jsonl", strings.NewReader(parentContent)); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "projects", "-workspace")
	ownKey := "tatara/repo/issue-7.jsonl"

	sid, err := Fork(ctx, st, "tatara/task-brainstorm.jsonl", ownKey, dir)
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if sid != parentSID {
		t.Errorf("Fork sid = %q, want %q", sid, parentSID)
	}
	// The parent was copied onto the issue's own key (diverging copy).
	if ok, _ := st.Exists(ctx, ownKey); !ok {
		t.Error("Fork must copy the parent onto the own key")
	}
	// The transcript is on disk under the session id for --resume.
	if _, err := os.Stat(filepath.Join(dir, parentSID+".jsonl")); err != nil {
		t.Errorf("forked transcript not written for resume: %v", err)
	}
}

func TestFork_NoParentIsNoop(t *testing.T) {
	sid, err := Fork(context.Background(), storage.NewMemStore(), "absent", "own", t.TempDir())
	if err != nil {
		t.Fatalf("Fork with absent parent: %v", err)
	}
	if sid != "" {
		t.Errorf("Fork with absent parent sid = %q, want empty", sid)
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
