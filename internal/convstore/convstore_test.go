package convstore

import "testing"

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
