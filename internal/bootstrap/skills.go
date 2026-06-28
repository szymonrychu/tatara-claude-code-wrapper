package bootstrap

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// SkillsCloneRetryDelay controls the sleep between clone retry attempts.
// Override in tests to keep the suite fast.
var SkillsCloneRetryDelay = func(attempt int) {
	time.Sleep(time.Duration(attempt) * 2 * time.Second)
}

// skillFrontmatter is the minimal YAML shape we care about in a SKILL.md header.
type skillFrontmatter struct {
	Profiles []string `yaml:"profiles"`
}

// parseProfiles extracts the profiles list from a SKILL.md YAML frontmatter
// block. Returns nil (treated as wildcard: install in any profile) when
// frontmatter is absent, malformed, or the profiles field is not present.
// Supports inline list form (profiles: ["a","b"]) and block list form, and
// CRLF line endings. Uses yaml.v3 so block-scalar values (description: >)
// cannot produce false matches.
func parseProfiles(skillMD []byte) []string {
	s := strings.ReplaceAll(string(skillMD), "\r\n", "\n")
	if !strings.HasPrefix(s, "---\n") {
		return nil
	}
	end := strings.Index(s[4:], "\n---")
	if end < 0 {
		return nil
	}
	fm := s[4 : 4+end]
	var out skillFrontmatter
	if err := yaml.Unmarshal([]byte(fm), &out); err != nil {
		return nil
	}
	return out.Profiles
}

// shouldInstall returns true when the skill should be installed for activeProfile.
// Rules (per contract):
//   - activeProfile == ""      -> install all (fail-open)
//   - profiles nil or empty    -> treat as wildcard, always install
//   - profiles contains "*"    -> always install
//   - profiles contains activeProfile -> install
//   - otherwise                -> skip
func shouldInstall(activeProfile string, profiles []string) bool {
	if activeProfile == "" {
		return true
	}
	if len(profiles) == 0 {
		return true
	}
	for _, p := range profiles {
		if p == "*" || p == activeProfile {
			return true
		}
	}
	return false
}

// cloneSkillsRepo shallow-clones SkillsRepo at SkillsRef into p.SkillsCloneDir.
// Uses the same GitRunner + GIT_TOKEN credential-helper pattern as the main
// repo clone. Retries 3x with exponential backoff. On total failure: logs WARN,
// increments the failure counter, and returns nil (fail-open: boot proceeds
// with whatever SkillsSrc entries exist or are already populated).
func cloneSkillsRepo(p Params, git GitRunner) error {
	repo := p.SkillsRepo
	if repo == "" || p.SkillsCloneDir == "" {
		return nil
	}
	ref := p.SkillsRef
	if ref == "" {
		ref = "main"
	}

	// Set credential helper for private repos; mirrors configureGit.
	if p.GitToken != "" {
		helper := `!f() { echo username=x-access-token; echo "password=$GIT_TOKEN"; }; f`
		if err := git("", "config", "--global", "credential.helper", helper); err != nil {
			if p.Log != nil {
				p.Log.Warn("skills clone: credential helper setup failed",
					"action", "skills_clone", "error", err)
			}
		}
	}

	// Skip when already cloned (pod restart with persistent clone dir).
	if _, err := os.Stat(filepath.Join(p.SkillsCloneDir, ".git")); err == nil {
		if p.Log != nil {
			p.Log.Info("skills repo already cloned, skipping",
				"action", "skills_clone", "dir", p.SkillsCloneDir)
		}
		return nil
	}

	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		// Remove any partial state from a previous failed attempt so that
		// "git init" does not see a pre-existing .git and so the .git stat
		// guard above cannot be tripped by a half-initialised directory.
		if attempt > 1 {
			_ = os.RemoveAll(p.SkillsCloneDir)
		}
		// SHA-ref-safe fetch sequence. "git clone -b <ref>" rejects raw commit
		// SHAs ("Remote branch <sha> not found"); "git fetch origin <ref>" accepts
		// branches, tags, and reachable commit SHAs uniformly (GitHub allows
		// fetching any reachable SHA with --depth 1).
		var stepErr error
		for _, step := range []func() error{
			func() error { return git("", "init", "-q", p.SkillsCloneDir) },
			func() error { return git(p.SkillsCloneDir, "remote", "add", "origin", repo) },
			func() error { return git(p.SkillsCloneDir, "fetch", "--depth", "1", "origin", ref) },
			func() error { return git(p.SkillsCloneDir, "checkout", "-q", "--detach", "FETCH_HEAD") },
		} {
			if stepErr = step(); stepErr != nil {
				break
			}
		}
		if stepErr == nil {
			if p.Log != nil {
				p.Log.Info("skills repo cloned", "action", "skills_clone",
					"repo", repo, "ref", ref, "dir", p.SkillsCloneDir)
			}
			return nil
		}
		lastErr = stepErr
		if p.Log != nil {
			p.Log.Warn("skills clone attempt failed", "action", "skills_clone",
				"attempt", attempt, "error", stepErr)
		}
		if attempt < 3 {
			SkillsCloneRetryDelay(attempt)
		}
	}

	if p.Log != nil {
		p.Log.Warn("skills clone failed after 3 attempts; continuing without cloned skills",
			"action", "skills_clone", "error", lastErr)
	}
	if p.M != nil {
		p.M.SkillsCloneFailures.Inc()
	}
	return nil
}

// installSkills copies skill dirs from each SkillsSrc directory into
// <workspace>/.claude/skills, filtered by p.SkillProfile. A skill dir is any
// directory that directly contains a SKILL.md file. Later sources win on name
// collision (custom overrides baked). An empty SkillProfile installs all skills
// (fail-open). Skills whose profiles frontmatter field does not include the
// active profile are skipped.
func installSkills(p Params) error {
	dst := filepath.Join(p.Workspace, ".claude", "skills")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("mkdir skills: %w", err)
	}
	total := 0
	for _, src := range p.SkillsSrc {
		if src == "" {
			continue
		}
		if _, err := os.Stat(src); os.IsNotExist(err) {
			continue
		}
		n, err := installSkillsFromSrc(src, dst, p)
		if err != nil {
			return fmt.Errorf("install skills from %s: %w", src, err)
		}
		total += n
	}
	if p.Log != nil {
		p.Log.Info("skills installed", "action", "install_skills",
			"count", total, "profile", p.SkillProfile)
	}
	if p.M != nil {
		p.M.SkillsInstalled.WithLabelValues(p.SkillProfile).Add(float64(total))
	}
	return nil
}

// installSkillsFromSrc walks src looking for skill dirs (dirs that directly
// contain SKILL.md). Each matched skill dir is filtered by profile and, if it
// passes, copied wholesale into dst, preserving the relative path from src.
func installSkillsFromSrc(src, dst string, p Params) (int, error) {
	installed := 0
	err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(src, path)
		if relErr != nil {
			return fmt.Errorf("rel %s: %w", path, relErr)
		}
		if rel == "." {
			return nil // source root is never a skill dir
		}
		b, readErr := os.ReadFile(filepath.Join(path, "SKILL.md"))
		if os.IsNotExist(readErr) {
			return nil // category dir; keep descending
		}
		if readErr != nil {
			return nil // unreadable SKILL.md; skip
		}
		profiles := parseProfiles(b)
		if !shouldInstall(p.SkillProfile, profiles) {
			if p.Log != nil {
				p.Log.Debug("skill skipped", "action", "install_skills",
					"skill", rel, "profile", p.SkillProfile)
			}
			return filepath.SkipDir
		}
		target := filepath.Join(dst, rel)
		if _, statErr := os.Stat(target); statErr == nil && p.Log != nil {
			p.Log.Info("skill shadowed", "action", "install_skills", "rel", rel, "src", src)
		}
		if err := copySkillDir(path, target); err != nil {
			return fmt.Errorf("copy skill %s: %w", rel, err)
		}
		if p.Log != nil {
			p.Log.Debug("skill installed", "action", "install_skills",
				"skill", rel, "profile", p.SkillProfile)
		}
		installed++
		return filepath.SkipDir // don't recurse into the skill dir itself
	})
	return installed, err
}

// copySkillDir copies the full directory tree at src into dst.
func copySkillDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return fmt.Errorf("rel %s: %w", path, err)
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return copyFile(path, target, info.Mode().Perm())
	})
}

func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()
	_, err = io.Copy(out, in)
	return err
}
