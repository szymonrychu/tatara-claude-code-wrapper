// Package promptguard discovers the agent-facing prompt text this repo emits
// and extracts the MCP tool names it references, so a test can assert every one
// of them is still advertised by the live MCP server.
//
// Why a source scan rather than a hand-maintained list of names: the bug this
// guards against (#136) is a hardcoded tool name outliving the tool it names.
// The injected global CLAUDE.md told a blocked agent to call comment_on_issue
// and decline_implementation for 14 days after tatara-cli reaped both. A list
// someone must remember to extend rots exactly the way that tool name did, so
// the scan finds the prompt blocks itself: a NEW prompt constant is covered the
// moment it is added, with no registration step to forget.
//
// The guard deliberately fails CLOSED. Every snake_case token inside a prompt
// block is treated as an MCP tool name and must appear in the live tools/list;
// there is no allowlist of "words that merely look like tool names". A
// validator that silently skips names it does not recognise is the failure mode
// filed as tatara-agent-skills#46, and it is precisely how a dead name survives
// review. If prompt prose ever needs a snake_case word that is not a tool, the
// guard fails and the prose gets reworded - that is the intended trade.
package promptguard

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// minPromptLen is the length floor a multi-line string literal must clear to
// count as agent-facing prompt text. Prompt blocks in this repo are hundreds of
// bytes of prose (the two injected CLAUDE.md directives are 741 and 607); the
// only other multi-line literals are 1-4 byte separators like "\n". 40 sits far
// below the real prompts and far above the separators, so it is sensitive
// without being noisy.
const minPromptLen = 40

// toolNameToken matches an MCP-tool-shaped identifier: lowercase, at least one
// underscore, e.g. issue_write / submit_outcome / report_internal_issue.
// Claude's own built-ins (AskUserQuestion, ExitPlanMode) are CamelCase and are
// correctly not matched - they are not MCP tools and have no tools/list entry.
var toolNameToken = regexp.MustCompile(`\b[a-z][a-z0-9]*(?:_[a-z0-9]+)+\b`)

// skipDirs are never scanned: no agent-facing prompt text lives in them.
var skipDirs = map[string]bool{".git": true, "vendor": true, "testdata": true, "bin": true, "dist": true}

// Literal is one multi-line string literal from a non-test Go source file,
// treated as agent-facing prompt text.
type Literal struct {
	File string
	Line int
	Text string
}

// Literals returns every prompt-shaped string literal under root, scanning
// non-test .go files only (a _test.go file emits nothing into an agent pod, and
// tests legitimately name retired tools when asserting they are gone).
//
// It returns an error when the scan finds nothing. A scanner that quietly
// matches zero files reports "no stale tool names" for the same reason a broken
// grep does, which would make this guard fail open.
func Literals(root string) ([]Literal, error) {
	var out []Literal
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", path, perr)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			bl, ok := n.(*ast.BasicLit)
			if !ok || bl.Kind != token.STRING {
				return true
			}
			s, uerr := strconv.Unquote(bl.Value)
			if uerr != nil || len(s) < minPromptLen || !strings.Contains(s, "\n") {
				return true
			}
			out = append(out, Literal{File: path, Line: fset.Position(bl.Pos()).Line, Text: s})
			return true
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("promptguard: no prompt-shaped literals found under %s; the scan matched nothing and would fail open", root)
	}
	return out, nil
}

// ToolNamesIn returns the sorted, de-duplicated MCP-tool-shaped tokens in text.
func ToolNamesIn(text string) []string {
	seen := map[string]bool{}
	for _, m := range toolNameToken.FindAllString(text, -1) {
		seen[m] = true
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ToolNames returns the sorted, de-duplicated tool names referenced across all
// the given literals.
func ToolNames(lits []Literal) []string {
	seen := map[string]bool{}
	for _, l := range lits {
		for _, n := range ToolNamesIn(l.Text) {
			seen[n] = true
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Where reports the file:line of the first literal referencing tool, for a
// failure message that points at the prose to fix.
func Where(lits []Literal, tool string) string {
	for _, l := range lits {
		for _, n := range ToolNamesIn(l.Text) {
			if n == tool {
				return fmt.Sprintf("%s:%d", l.File, l.Line)
			}
		}
	}
	return "unknown location"
}

// RepoRoot returns the module root (the directory holding go.mod), found by
// walking up from this source file. Works both in a checkout and in the
// Dockerfile guard stage, where the source is copied to /src.
func RepoRoot() (string, error) {
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("promptguard: runtime.Caller failed")
	}
	dir := filepath.Dir(self)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("promptguard: no go.mod above %s", filepath.Dir(self))
		}
		dir = parent
	}
}
