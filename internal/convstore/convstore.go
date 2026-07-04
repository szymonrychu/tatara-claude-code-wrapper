// Package convstore derives the on-disk transcript directory Claude Code
// reads/writes session transcripts in for a given working directory. It
// backs session.shouldResume's on-disk transcript check (the boot-crash fix).
// The S3-backed restore/upload/fork machinery this package used to hold
// (issue #114) was removed by the handoff-continuation design (component 3):
// continuation is now carried by a compact chat-backed handoff, not a
// replayed transcript. The on-disk layout is pinned by the spike in
// docs/conversation-resume-spike.md: a transcript lives at
// ~/.claude/projects/<ProjectDirName(cwd)>/<sessionId>.jsonl.
package convstore

import "path/filepath"

// ProjectDirName mirrors how Claude Code derives the per-cwd transcript
// directory under ~/.claude/projects: every non-alphanumeric rune in the
// absolute cwd becomes '-'. E.g. "/workspace" -> "-workspace",
// "/tmp/spike-ws" -> "-tmp-spike-ws". Verified empirically (see the spike doc);
// it is deterministic with no hashing, so it is identical across pods that share
// a cwd.
func ProjectDirName(cwd string) string {
	out := []rune(cwd)
	for i, r := range out {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			continue
		}
		out[i] = '-'
	}
	return string(out)
}

// TranscriptDir is ~/.claude/projects/<ProjectDirName(cwd)>, the directory Claude
// reads/writes session transcripts in for the given working directory. Used by
// session.shouldResume's transcriptExistsOnDisk to detect a resumable
// mid-first-turn crash.
func TranscriptDir(homeDir, cwd string) string {
	return filepath.Join(homeDir, ".claude", "projects", ProjectDirName(cwd))
}
