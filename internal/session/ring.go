package session

import (
	"regexp"
	"strings"
	"sync"
)

const ringCap = 64 * 1024

var (
	reCSI   = regexp.MustCompile(`\x1b\[[0-9;?:]*[ -/]*[@-~]`)
	reOSC   = regexp.MustCompile("\x1b\\][^\x07\x1b]*(\x07|\x1b\\\\)?")
	reOther = regexp.MustCompile(`\x1b[@-Z\\-_=>]`)
)

// ringBuffer holds the last ringCap bytes of PTY output. It is the only window
// into the interactive TUI: used to detect boot dialogs and for debug logging.
type ringBuffer struct {
	mu    sync.Mutex
	buf   []byte
	total int // monotonic count of bytes ever written (boot-quiescence signal)
}

func newRing() *ringBuffer { return &ringBuffer{} }

func (r *ringBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if len(r.buf) > ringCap {
		r.buf = r.buf[len(r.buf)-ringCap:]
	}
	r.total += len(p)
	return len(p), nil
}

// written returns the monotonic byte count, used to detect when output settles.
func (r *ringBuffer) written() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.total
}

// contains reports whether the de-ANSI'd, whitespace-stripped buffer contains
// the whitespace-stripped needle. The TUI lays out text with cursor-move
// escapes, so on-screen words carry no literal spaces once stripped - matching
// must ignore whitespace.
func (r *ringBuffer) contains(needle string) bool {
	r.mu.Lock()
	s := string(r.buf)
	r.mu.Unlock()
	return strings.Contains(stripWS(stripANSI(s)), stripWS(needle))
}

// tail returns up to n trailing bytes, de-ANSI'd, for debug logging.
func (r *ringBuffer) tail(n int) string {
	r.mu.Lock()
	s := stripANSI(string(r.buf))
	r.mu.Unlock()
	if len(s) > n {
		s = s[len(s)-n:]
	}
	return s
}

func stripANSI(s string) string {
	s = reCSI.ReplaceAllString(s, "")
	s = reOSC.ReplaceAllString(s, "")
	s = reOther.ReplaceAllString(s, "")
	var b strings.Builder
	for _, c := range s {
		if c == 0x1b || (c < 0x20 && c != '\n' && c != '\t') {
			continue
		}
		b.WriteRune(c)
	}
	return b.String()
}

func stripWS(s string) string {
	var b strings.Builder
	for _, c := range s {
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			b.WriteRune(c)
		}
	}
	return b.String()
}
