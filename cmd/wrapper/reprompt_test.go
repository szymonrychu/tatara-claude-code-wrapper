package main

import (
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestReprompt_BudgetAndRollback(t *testing.T) {
	t.Run("re-prompts up to the cap then refuses", func(t *testing.T) {
		var calls int
		a := &app{log: testLogger(), submitFn: func(text, cb string) (string, error) {
			calls++
			require.Contains(t, text, "rejected by the operator")
			require.Contains(t, text, "non-blank")
			require.Equal(t, "https://cb/x", cb)
			return "turn-1", nil
		}}
		require.True(t, a.reprompt("decline_implementation", "400: blank reason", "https://cb/x"))
		require.True(t, a.reprompt("decline_implementation", "400", "https://cb/x"))
		require.False(t, a.reprompt("decline_implementation", "400", "https://cb/x"),
			"third re-prompt must be refused once the budget is exhausted")
		require.Equal(t, maxOutcomeReprompts, calls)
	})

	t.Run("submit failure rolls back the budget", func(t *testing.T) {
		fail := true
		var calls int
		a := &app{log: testLogger(), submitFn: func(text, cb string) (string, error) {
			calls++
			if fail {
				return "", errors.New("session busy")
			}
			return "turn-2", nil
		}}
		require.False(t, a.reprompt("already_done", "400", ""), "a failed submit must not count against the budget")
		fail = false
		require.True(t, a.reprompt("already_done", "400", ""), "budget must be intact after a failed submit")
		require.Equal(t, 2, calls)
	})

	t.Run("concurrent re-prompts never exceed the cap", func(t *testing.T) {
		var mu sync.Mutex
		var calls int
		a := &app{log: testLogger(), submitFn: func(text, cb string) (string, error) {
			mu.Lock()
			calls++
			mu.Unlock()
			return "t", nil
		}}
		var wg sync.WaitGroup
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); a.reprompt("decline_implementation", "e", "") }()
		}
		wg.Wait()
		require.LessOrEqual(t, calls, maxOutcomeReprompts)
	})
}
