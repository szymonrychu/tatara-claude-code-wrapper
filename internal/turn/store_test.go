package turn_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/turn"
)

func TestStore_CreateCompleteGet(t *testing.T) {
	s := turn.NewStore()
	t0 := time.Unix(1000, 0)
	rec := s.Create("turn-1", "hello", "https://cb.example/x", t0)
	require.Equal(t, turn.Running, rec.State)

	got, ok := s.Get("turn-1")
	require.True(t, ok)
	require.Equal(t, turn.Running, got.State)
	require.Equal(t, "https://cb.example/x", got.CallbackURL)

	t1 := time.Unix(1005, 0)
	require.NoError(t, s.Complete("turn-1", "PONG",
		json.RawMessage(`{"status":"success"}`), json.RawMessage(`{"output_tokens":3}`), "end_turn", t1))

	got, _ = s.Get("turn-1")
	require.Equal(t, turn.Complete, got.State)
	require.Equal(t, "PONG", got.FinalText)
	require.Equal(t, "end_turn", got.StopReason)
	require.NotNil(t, got.CompletedAt)

	require.ErrorIs(t, s.Complete("missing", "", nil, nil, "", t1), turn.ErrNotFound)
}

func TestStore_ListOrderedAndFail(t *testing.T) {
	s := turn.NewStore()
	s.Create("a", "1", "", time.Unix(1, 0))
	s.Create("b", "2", "", time.Unix(2, 0))
	require.NoError(t, s.Fail("a", "boom", time.Unix(3, 0)))

	list := s.List()
	require.Len(t, list, 2)
	require.Equal(t, "a", list[0].ID)
	require.Equal(t, turn.Failed, list[0].State)
	require.Equal(t, "b", list[1].ID)
}
