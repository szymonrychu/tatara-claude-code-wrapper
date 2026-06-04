//go:build ignore

// stub claude: reads bracketed-paste blocks from stdin (the PTY slave); for
// each submitted message, appends an assistant line to $STUB_TRANSCRIPT and
// runs $STUB_HOOK with a synthetic hook payload on its stdin.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func main() {
	tp := os.Getenv("STUB_TRANSCRIPT")
	hook := os.Getenv("STUB_HOOK")
	fmt.Println("stub-claude ready") // readiness marker
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		line := sc.Text()
		msg := strings.TrimSuffix(strings.TrimPrefix(line, "\x1b[200~"), "\x1b[201~")
		if msg == "" {
			continue
		}
		f, _ := os.OpenFile(tp, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		al := map[string]any{"type": "assistant", "message": map[string]any{
			"content": []map[string]any{{"type": "text", "text": "echo:" + msg}},
			"usage":   map[string]any{"output_tokens": 1}}}
		b, _ := json.Marshal(al)
		_, _ = f.Write(append(b, '\n'))
		_ = f.Close()

		payload, _ := json.Marshal(map[string]any{
			"session_id":             "stub",
			"transcript_path":        tp,
			"stop_reason":            "end_turn",
			"last_assistant_message": "echo:" + msg,
		})
		c := exec.Command(hook)
		c.Stdin = strings.NewReader(string(payload))
		c.Env = os.Environ()
		_ = c.Run()
	}
}
