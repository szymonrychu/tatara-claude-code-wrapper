package transcript

import (
	"testing"
)

func TestRedactor_ScrubReplacesSecret(t *testing.T) {
	r := NewRedactor(map[string]string{
		"MY_TOKEN": "supersecrettoken123",
	})
	got := r.Scrub("the token is supersecrettoken123 in the text")
	want := "the token is [REDACTED:MY_TOKEN] in the text"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRedactor_SkipsShortValues(t *testing.T) {
	r := NewRedactor(map[string]string{
		"SHORT": "abc",
	})
	got := r.Scrub("the value abc is present")
	if got != "the value abc is present" {
		t.Fatalf("expected no redaction for short value, got %q", got)
	}
}

func TestRedactor_LongestFirst(t *testing.T) {
	// "longersecret" contains "secret" - longest-first means "longersecret" is replaced,
	// not the shorter "secret" substring.
	r := NewRedactor(map[string]string{
		"A": "longersecret",
		"B": "secret",
	})
	got := r.Scrub("value is longersecret here")
	// Must be redacted as A (longer), not partially as B
	if got != "value is [REDACTED:A] here" {
		t.Fatalf("expected longest-first replacement, got %q", got)
	}
}

func TestRedactor_MultipleSecrets(t *testing.T) {
	r := NewRedactor(map[string]string{
		"TOKEN_A": "tokenAlpha1234",
		"TOKEN_B": "tokenBeta5678",
	})
	got := r.Scrub("a=tokenAlpha1234 b=tokenBeta5678")
	want := "a=[REDACTED:TOKEN_A] b=[REDACTED:TOKEN_B]"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRedactor_EmptyInput(t *testing.T) {
	r := NewRedactor(map[string]string{
		"MY_TOKEN": "supersecrettoken123",
	})
	got := r.Scrub("")
	if got != "" {
		t.Fatalf("expected empty string, got %q", got)
	}
}

func TestNewRedactor_SkipsBoundaryValue(t *testing.T) {
	// exactly 7 chars - below the 8-char threshold
	r := NewRedactor(map[string]string{
		"K": "1234567",
	})
	got := r.Scrub("value 1234567 present")
	if got != "value 1234567 present" {
		t.Fatalf("expected no redaction for 7-char value, got %q", got)
	}
}

func TestNewRedactor_IncludesBoundaryValue(t *testing.T) {
	// exactly 8 chars - meets the 8-char threshold
	r := NewRedactor(map[string]string{
		"K": "12345678",
	})
	got := r.Scrub("value 12345678 present")
	if got != "value [REDACTED:K] present" {
		t.Fatalf("expected redaction for 8-char value, got %q", got)
	}
}
