package bootstrap

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// installSkills copies every skill tree under each SkillsSrc dir into
// /workspace/.claude/skills, so baked + custom skills coexist.
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
		if err := copyTree(src, dst); err != nil {
			return fmt.Errorf("install skills from %s: %w", src, err)
		}
	}
	return nil
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return copyFile(path, target)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()
	_, err = io.Copy(out, in)
	return err
}
