package bootstrap

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// extraSkillSourceNameRE constrains extraSkillSource.Name before it is ever
// used to build a filesystem path (filepath.Join(base, Name)). Anything
// outside [a-z0-9-]+ - in particular "..", "/", and leading "-" - is rejected
// so os.RemoveAll/clone can never touch a path outside base.
var extraSkillSourceNameRE = regexp.MustCompile(`^[a-z0-9-]+$`)

// SkillsCloneRetryDelay controls the sleep between clone retry attempts.
// Override in tests to keep the suite fast.
var SkillsCloneRetryDelay = func(attempt int) {
	time.Sleep(time.Duration(attempt) * 2 * time.Second)
}

// skillsCloneSentinel marks a skills clone as COMPLETE and records the ref it
// holds. It is written only after "checkout --detach" succeeds, so its presence
// is proof of a finished tree in a way a bare .git is not: a kill between "git
// init" and the checkout leaves .git behind with an empty tree, and with a
// persistent clone dir a .git-keyed skip would make every later boot in that
// container install zero skills - silently, since cloneSkillsRepo would return
// nil and never touch the failure counter (issue #173).
const skillsCloneSentinel = ".tatara-skills-ref"

// Label values for Metrics.SkillsCloneFailures. Splitting the two keeps a bad
// TATARA_EXTRA_SKILL_SOURCES entry, which is a per-project config error, off
// the series that means the fleet-wide harness clone is failing.
const (
	skillsCloneSourceRepo  = "skills_repo"
	skillsCloneSourceExtra = "extra"
)

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

	// Reuse an existing clone only when the completion sentinel says the tree is
	// finished AND holds the ref we were asked for. Anything else - no sentinel,
	// a stale ref, an unreadable file - is re-cloned from scratch below.
	if b, err := os.ReadFile(filepath.Join(p.SkillsCloneDir, skillsCloneSentinel)); err == nil &&
		strings.TrimSpace(string(b)) == ref {
		if p.Log != nil {
			p.Log.Info("skills repo already cloned at requested ref, skipping",
				"action", "skills_clone", "dir", p.SkillsCloneDir, "ref", ref)
		}
		return nil
	}

	// The retry loop below clears the target with os.RemoveAll, and the target
	// is not a fixed path: it is filepath.Dir of the first SKILLS_SRC_DIRS
	// entry, which a Project can override through ExtraEnvs. Bound the delete
	// to a directory that is plausibly ours before the loop can run, so a value
	// like SKILLS_SRC_DIRS=/etc/wrapper/skills - which resolves the clone dir to
	// the wrapper's own mounted /etc/wrapper - cannot wipe the pod's config.
	// Fail-open by design: the zero-skill check in Render is what turns a
	// refused clone into a boot failure, and it keys on the installed count.
	if !clonableDir(p.SkillsCloneDir) {
		if p.Log != nil {
			p.Log.Warn("skills clone target is not empty and carries no clone marker; refusing to clear it",
				"action", "skills_clone", "dir", p.SkillsCloneDir)
		}
		if p.M != nil {
			p.M.SkillsCloneFailures.WithLabelValues(skillsCloneSourceRepo).Inc()
		}
		return nil
	}

	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		// The directory contents are untrusted on every attempt: we only get
		// here when the sentinel did not vouch for them, so they may be a
		// half-initialised clone from a killed boot or a checkout of a
		// different ref. Clear it so "git init"/"remote add" start clean. The
		// clonableDir guard above is what makes this safe to do unconditionally.
		_ = os.RemoveAll(p.SkillsCloneDir)
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
			// Sentinel last: it is the only thing a later boot trusts. A failure
			// to write it costs a redundant re-clone, never a skipped one, so it
			// warns rather than failing the boot.
			if err := writeSkillsCloneSentinel(p.SkillsCloneDir, ref); err != nil && p.Log != nil {
				p.Log.Warn("skills clone: sentinel write failed; next boot will re-clone",
					"action", "skills_clone", "error", err)
			}
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
		p.M.SkillsCloneFailures.WithLabelValues(skillsCloneSourceRepo).Inc()
	}
	return nil
}

// clonableDir reports whether dir is safe for the retry loop's os.RemoveAll:
// absent (we create it), empty, or already carrying a clone marker - a .git
// from a completed or half-initialised clone, or the completion sentinel.
// Anything else is somebody else's directory and is left alone. An unreadable
// path (including a non-directory) is not clonable either: we do not delete
// what we cannot inspect.
func clonableDir(dir string) bool {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return os.IsNotExist(err)
	}
	for _, e := range ents {
		if e.Name() == ".git" || e.Name() == skillsCloneSentinel {
			return true
		}
	}
	return len(ents) == 0
}

func writeSkillsCloneSentinel(dir, ref string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, skillsCloneSentinel), []byte(ref+"\n"), 0o644)
}

// extraSkillSource is one entry of TATARA_EXTRA_SKILL_SOURCES: a Project-scoped
// extra skill repository the wrapper clones and installs skills from.
type extraSkillSource struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Ref    string `json:"ref"`
	Subdir string `json:"subdir"`
}

// parseExtraSkillSources parses the TATARA_EXTRA_SKILL_SOURCES JSON array.
// Fail-open: returns nil on empty, whitespace-only, or malformed input (never
// errors the boot).
func parseExtraSkillSources(raw []byte) []extraSkillSource {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var out []extraSkillSource
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

// cloneExtraSkillSource shallow-clones one extra skill source into dir, using
// the same SHA-ref-safe init/remote/fetch/checkout sequence as cloneSkillsRepo
// (so branches, tags, and reachable commit SHAs all work uniformly). Auth
// reuses the global GIT_TOKEN credential helper already configured by
// configureGit/cloneSkillsRepo before this runs; no separate credential setup
// is needed here.
func cloneExtraSkillSource(git GitRunner, s extraSkillSource, dir string) error {
	ref := s.Ref
	if ref == "" {
		ref = "main"
	}
	// Skip when already cloned (pod restart with persistent workspace).
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		return nil
	}
	_ = os.RemoveAll(dir)
	for _, step := range []func() error{
		func() error { return git("", "init", "-q", dir) },
		func() error { return git(dir, "remote", "add", "origin", s.URL) },
		func() error { return git(dir, "fetch", "--depth", "1", "origin", ref) },
		func() error { return git(dir, "checkout", "-q", "--detach", "FETCH_HEAD") },
	} {
		if err := step(); err != nil {
			return err
		}
	}
	return nil
}

// installExtraSkillSources clones each TATARA_EXTRA_SKILL_SOURCES entry and
// installs skills from <clone>/<subdir> via installSkillsFromSrc (profiles:
// gating unchanged). Runs AFTER the baked skills (installSkills) so a project
// source can override a same-named baked skill. Each source is validated
// before any filesystem or network use: Name against extraSkillSourceNameRE,
// Name uniqueness, URL scheme (http/https only), and Subdir containment
// (rejects absolute paths and "../" escapes). One failing source (bad
// name/url/subdir, duplicate name, clone failure, or install failure) logs
// WARN, increments the clone-failure counter on a clone failure, and the
// rest proceed: per-source failure isolation, never fatal. Always returns
// nil (fail-open).
func installExtraSkillSources(p Params, git GitRunner) error {
	sources := parseExtraSkillSources(p.ExtraSkillSources)
	if len(sources) == 0 {
		return nil
	}
	dst := filepath.Join(p.Workspace, ".claude", "skills")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		if p.Log != nil {
			p.Log.Warn("extra skill sources: mkdir skills failed", "action", "extra_skills", "error", err)
		}
		return nil
	}
	base := filepath.Join(p.Workspace, ".extra-skills")
	total := 0
	seen := make(map[string]bool, len(sources))
	for _, s := range sources {
		if s.Name == "" || s.URL == "" {
			if p.Log != nil {
				p.Log.Warn("extra skill source missing name/url; skipping", "action", "extra_skills")
			}
			continue
		}
		// Name gates every filesystem use below (filepath.Join, os.RemoveAll via
		// cloneExtraSkillSource): reject before any path is built from it.
		if !extraSkillSourceNameRE.MatchString(s.Name) {
			if p.Log != nil {
				p.Log.Warn("extra skill source invalid name; skipping",
					"action", "extra_skills", "source", s.Name)
			}
			continue
		}
		if seen[s.Name] {
			if p.Log != nil {
				p.Log.Warn("duplicate extra skill source name; skipping",
					"action", "extra_skills", "source", s.Name)
			}
			continue
		}
		seen[s.Name] = true
		if !strings.HasPrefix(s.URL, "https://") && !strings.HasPrefix(s.URL, "http://") {
			if p.Log != nil {
				p.Log.Warn("extra skill source invalid url; skipping",
					"action", "extra_skills", "source", s.Name)
			}
			continue
		}
		// Subdir must stay inside cloneDir: reject absolute paths and any
		// cleaned form that escapes via "..", before it is ever joined in.
		if s.Subdir != "" && (filepath.IsAbs(s.Subdir) || strings.HasPrefix(filepath.Clean(s.Subdir), "..")) {
			if p.Log != nil {
				p.Log.Warn("extra skill source invalid subdir; skipping",
					"action", "extra_skills", "source", s.Name)
			}
			continue
		}
		cloneDir := filepath.Join(base, s.Name)
		if err := cloneExtraSkillSource(git, s, cloneDir); err != nil {
			if p.Log != nil {
				p.Log.Warn("extra skill source clone failed; skipping",
					"action", "extra_skills", "source", s.Name, "error", err)
			}
			if p.M != nil {
				p.M.SkillsCloneFailures.WithLabelValues(skillsCloneSourceExtra).Inc()
			}
			continue
		}
		src := cloneDir
		if s.Subdir != "" {
			src = filepath.Join(cloneDir, s.Subdir)
		}
		n, err := installSkillsFromSrc(src, dst, p)
		if err != nil {
			if p.Log != nil {
				p.Log.Warn("extra skill source install failed; skipping",
					"action", "extra_skills", "source", s.Name, "error", err)
			}
			continue
		}
		if p.Log != nil {
			p.Log.Info("extra skill source installed",
				"action", "extra_skills", "source", s.Name, "count", n)
		}
		total += n
	}
	if p.M != nil {
		p.M.SkillsInstalled.WithLabelValues(p.SkillProfile).Add(float64(total))
	}
	return nil
}

// installSkills copies skill dirs from each SkillsSrc directory into
// <workspace>/.claude/skills, filtered by p.SkillProfile. A skill dir is any
// directory that directly contains a SKILL.md file. Later sources win on name
// collision (custom overrides baked). An empty SkillProfile installs all skills
// (fail-open). Skills whose profiles frontmatter field does not include the
// active profile are skipped.
//
// Returns the number of skills installed. Render treats zero as fatal: it is
// the only predicate that survives every way the harness skills can go missing
// (clone failure, half-initialised clone dir, empty tree, profile matching
// nothing) - notably including the ones where cloneSkillsRepo still returns nil.
func installSkills(p Params) (int, error) {
	dst := filepath.Join(p.Workspace, ".claude", "skills")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return 0, fmt.Errorf("mkdir skills: %w", err)
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
			return total, fmt.Errorf("install skills from %s: %w", src, err)
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
	return total, nil
}

// hasSkillsSrc reports whether any skill source is configured at all. When none
// is, the zero-skill check is skipped: a deployment that deliberately runs
// skill-less is not a broken boot.
func hasSkillsSrc(p Params) bool {
	for _, src := range p.SkillsSrc {
		if src != "" {
			return true
		}
	}
	return false
}

// installSkillsFromSrc walks src looking for skill dirs (dirs that directly
// contain SKILL.md). Each matched skill dir is filtered by profile and, if it
// passes, copied wholesale into dst FLATTENED to the skill dir's basename
// (dst/<skill>), dropping any category nesting from the source layout. Claude
// Code only discovers skills one level under .claude/skills.
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
		target := filepath.Join(dst, filepath.Base(path))
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
