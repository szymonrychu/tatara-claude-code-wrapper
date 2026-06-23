// Package convstore wires the S3 storage client into the wrapper's conversation
// lifecycle (issue #114): restore a prior conversation transcript on boot so a
// fresh pod resumes via `claude --resume <sid>`, and upload the live transcript
// after each turn so the next pod can pick it up.
//
// The restore/upload logic is kept here (not in bootstrap) so the bootstrap
// package stays free of the cloud SDK. The on-disk layout is pinned by the
// spike in docs/conversation-resume-spike.md: a transcript lives at
// ~/.claude/projects/<ProjectDirName(cwd)>/<sessionId>.jsonl, and placing the
// blob there is sufficient for resume.
package convstore

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/storage"
)

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
// reads/writes session transcripts in for the given working directory.
func TranscriptDir(homeDir, cwd string) string {
	return filepath.Join(homeDir, ".claude", "projects", ProjectDirName(cwd))
}

// SessionIDFromPath extracts the Claude session id from a transcript path:
// ~/.claude/projects/<dir>/<sessionId>.jsonl -> <sessionId>. claude names the
// transcript file after the session id, so the basename is authoritative.
func SessionIDFromPath(transcriptPath string) string {
	return strings.TrimSuffix(filepath.Base(transcriptPath), ".jsonl")
}

// Upload stores the transcript file at transcriptPath under key. The caller logs
// and meters the result; an upload failure must never fail the turn.
func Upload(ctx context.Context, store storage.Store, key, transcriptPath string) error {
	f, err := os.Open(transcriptPath) //nolint:gosec // transcriptPath is claude-controlled, from the Stop hook
	if err != nil {
		return fmt.Errorf("open transcript %s: %w", transcriptPath, err)
	}
	defer func() { _ = f.Close() }()
	if err := store.Put(ctx, key, f); err != nil {
		return fmt.Errorf("upload conversation %s: %w", key, err)
	}
	return nil
}

// ParseSessionID reads the Claude session id from transcript JSONL content. Each
// line carries a "sessionId" field; the first non-empty one wins. Returns "" if
// none is found.
func ParseSessionID(r io.Reader) string {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		var line struct {
			SessionID string `json:"sessionId"`
		}
		if err := json.Unmarshal(sc.Bytes(), &line); err == nil && line.SessionID != "" {
			return line.SessionID
		}
	}
	return ""
}

// Fork copies the parent conversation at parentKey to ownKey (so each issue gets
// its own diverging copy, issue #114 decision 3), writes it to disk under its
// session id, and returns that session id so the caller can launch
// `claude --resume <sid>`. The copy keys the conversation by issue while leaving
// the parent untouched. Returns ("", nil) when parentKey is absent (nothing to
// fork).
func Fork(ctx context.Context, store storage.Store, parentKey, ownKey, transcriptDir string) (string, error) {
	exists, err := store.Exists(ctx, parentKey)
	if err != nil {
		return "", fmt.Errorf("check fork parent %s: %w", parentKey, err)
	}
	if !exists {
		return "", nil
	}
	if err := store.Copy(ctx, parentKey, ownKey); err != nil {
		return "", fmt.Errorf("fork copy %s -> %s: %w", parentKey, ownKey, err)
	}
	rc, err := store.Get(ctx, ownKey)
	if err != nil {
		return "", fmt.Errorf("read forked conversation %s: %w", ownKey, err)
	}
	body, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		return "", fmt.Errorf("read forked conversation %s: %w", ownKey, err)
	}
	sid := ParseSessionID(bytes.NewReader(body))
	if sid == "" {
		return "", fmt.Errorf("forked conversation %s has no sessionId", ownKey)
	}
	if err := os.MkdirAll(transcriptDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir transcript dir %s: %w", transcriptDir, err)
	}
	dest := filepath.Join(transcriptDir, sid+".jsonl")
	if err := os.WriteFile(dest, body, 0o600); err != nil { //nolint:gosec // dest derived from parsed sessionId + home, not user input
		return "", fmt.Errorf("write forked transcript %s: %w", dest, err)
	}
	return sid, nil
}

// Restore downloads the conversation blob at key into
// <transcriptDir>/<sessionID>.jsonl so `claude --resume <sessionID>` finds it on
// boot. It returns (false, nil) when the object is absent (the first run of a
// conversation: nothing to restore). A non-nil error leaves the caller to start
// fresh.
func Restore(ctx context.Context, store storage.Store, key, sessionID, transcriptDir string) (bool, error) {
	exists, err := store.Exists(ctx, key)
	if err != nil {
		return false, fmt.Errorf("check conversation %s: %w", key, err)
	}
	if !exists {
		return false, nil
	}
	rc, err := store.Get(ctx, key)
	if err != nil {
		return false, fmt.Errorf("download conversation %s: %w", key, err)
	}
	defer func() { _ = rc.Close() }()
	if err := os.MkdirAll(transcriptDir, 0o755); err != nil {
		return false, fmt.Errorf("mkdir transcript dir %s: %w", transcriptDir, err)
	}
	dest := filepath.Join(transcriptDir, sessionID+".jsonl")
	f, err := os.Create(dest) //nolint:gosec // dest is derived from operator-set sessionID + home, not user input
	if err != nil {
		return false, fmt.Errorf("create transcript %s: %w", dest, err)
	}
	if _, err := io.Copy(f, rc); err != nil {
		_ = f.Close()
		return false, fmt.Errorf("write transcript %s: %w", dest, err)
	}
	if err := f.Close(); err != nil {
		return false, fmt.Errorf("close transcript %s: %w", dest, err)
	}
	return true, nil
}
