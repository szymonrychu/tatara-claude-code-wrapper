package bootstrap

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiscoverySkillsPresentAndValid(t *testing.T) {
	root := "../../templates/skills"
	for _, name := range []string{"tatara-deep-research", "tatara-research-followup", "tatara-health-check"} {
		path := filepath.Join(root, name, "SKILL.md")
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		s := string(b)
		if !strings.HasPrefix(s, "---\n") {
			t.Fatalf("%s: missing YAML frontmatter", name)
		}
		end := strings.Index(s[4:], "\n---")
		if end < 0 {
			t.Fatalf("%s: unterminated frontmatter", name)
		}
		fm := s[4 : 4+end]
		if !strings.Contains(fm, "name: "+name) {
			t.Fatalf("%s: frontmatter name does not match dir", name)
		}
		if !strings.Contains(fm, "description:") {
			t.Fatalf("%s: frontmatter missing description", name)
		}
		body := s[4+end+4:]
		if len(strings.TrimSpace(body)) == 0 {
			t.Fatalf("%s: empty body", name)
		}
	}
}

// TestInstallSkills_PreservesExecutableBit verifies that copyFile/copyTree keep
// the source file mode (finding 1: exec bit on skill scripts must survive).
func TestInstallSkills_PreservesExecutableBit(t *testing.T) {
	src := t.TempDir()
	skillDir := filepath.Join(src, "brainstorming", "scripts")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(skillDir, "start-server.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	if err := installSkills(Params{Workspace: ws, SkillsSrc: []string{src}}); err != nil {
		t.Fatalf("installSkills: %v", err)
	}
	dst := filepath.Join(ws, ".claude", "skills", "brainstorming", "scripts", "start-server.sh")
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat copied script: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("exec bit lost: got mode %o, expected at least 0o111 set", info.Mode().Perm())
	}
}

func TestInstallSkills_CopiesDeployHarness(t *testing.T) {
	src := t.TempDir()
	// mimic the baked layout: <src>/tatara-deploy-harness/SKILL.md
	skillDir := filepath.Join(src, "tatara-deploy-harness")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: tatara-deploy-harness\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	if err := installSkills(Params{Workspace: ws, SkillsSrc: []string{src}}); err != nil {
		t.Fatalf("installSkills: %v", err)
	}
	got := filepath.Join(ws, ".claude", "skills", "tatara-deploy-harness", "SKILL.md")
	if _, err := os.Stat(got); err != nil {
		t.Fatalf("expected baked skill at %s: %v", got, err)
	}
}

// TestInstallSkills_CopiesDiscoverySkills asserts that the two wrapper agent
// skills (tatara-deep-research and tatara-research-followup) install correctly
// via installSkills, matching the baked templates/skills layout.
// TestInstallSkills_LogsShadowedSkill asserts that when two SkillsSrc dirs
// contain the same skill file, a log entry is emitted for the shadowed file
// (finding 4: silent overwrite must be visible in logs).
func TestInstallSkills_LogsShadowedSkill(t *testing.T) {
	src1 := t.TempDir()
	src2 := t.TempDir()
	// Both sources contain the same skill file.
	for _, src := range []string{src1, src2} {
		d := filepath.Join(src, "my-skill")
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte("# content from "+src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ws := t.TempDir()
	var logBuf strings.Builder
	log := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	if err := installSkills(Params{Workspace: ws, SkillsSrc: []string{src1, src2}, Log: log}); err != nil {
		t.Fatalf("installSkills: %v", err)
	}
	if !strings.Contains(logBuf.String(), "my-skill") {
		t.Fatalf("expected a shadow log mentioning skill name, got: %q", logBuf.String())
	}
}

func TestDiscoverySkillsCarryOrchestrationGuidance(t *testing.T) {
	root := "../../templates/skills"
	for _, name := range []string{"tatara-deep-research", "tatara-health-check"} {
		b, err := os.ReadFile(filepath.Join(root, name, "SKILL.md"))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		s := string(b)
		for _, want := range []string{
			"## Orchestration",
			"maximum effort",
			"one parallel subagent per repo",
			"Workflow",
		} {
			if !strings.Contains(s, want) {
				t.Fatalf("%s: orchestration guidance missing %q", name, want)
			}
		}
	}
}

func TestInstallSkills_CopiesDiscoverySkills(t *testing.T) {
	for _, name := range []string{"tatara-deep-research", "tatara-research-followup"} {
		name := name
		t.Run(name, func(t *testing.T) {
			src := t.TempDir()
			skillDir := filepath.Join(src, name)
			if err := os.MkdirAll(skillDir, 0o755); err != nil {
				t.Fatal(err)
			}
			stub := "---\nname: " + name + "\ndescription: stub\n---\n# body\n"
			if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(stub), 0o644); err != nil {
				t.Fatal(err)
			}
			ws := t.TempDir()
			if err := installSkills(Params{Workspace: ws, SkillsSrc: []string{src}}); err != nil {
				t.Fatalf("installSkills: %v", err)
			}
			got := filepath.Join(ws, ".claude", "skills", name, "SKILL.md")
			b, err := os.ReadFile(got)
			if err != nil {
				t.Fatalf("expected installed skill at %s: %v", got, err)
			}
			if !strings.Contains(string(b), "name: "+name) {
				t.Fatalf("%s: installed SKILL.md does not contain expected name", name)
			}
		})
	}
}
