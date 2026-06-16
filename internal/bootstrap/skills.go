package bootstrap

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// installSkills copies every skill tree under each SkillsSrc dir into
// /workspace/.claude/skills, so baked + custom skills coexist. Sources are
// processed in order; later sources win on name collision (custom overrides
// baked). A debug log is emitted when an existing target is overwritten so
// the shadowing is visible in agent logs.
func installSkills(p Params) error {
	dst := filepath.Join(p.Workspace, ".claude", "skills")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("mkdir skills: %w", err)
	}
	for _, src := range p.SkillsSrc {
		if src == "" {
			continue
		}
		if _, err := os.Stat(src); os.IsNotExist(err) {
			continue
		}
		if err := copyTreeWithLog(src, dst, p); err != nil {
			return fmt.Errorf("install skills from %s: %w", src, err)
		}
	}
	return nil
}

func copyTreeWithLog(src, dst string, p Params) error {
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
		if _, statErr := os.Stat(target); statErr == nil && p.Log != nil {
			// Target exists: later source is shadowing an earlier one.
			p.Log.Info("skill shadowed", "action", "install_skills", "rel", rel, "src", src)
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
