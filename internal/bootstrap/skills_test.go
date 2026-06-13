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
