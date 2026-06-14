package bootstrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiscoverySkillsPresentAndValid(t *testing.T) {
	root := "../../templates/skills"
	for _, name := range []string{"tatara-deep-research", "tatara-research-followup"} {
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
