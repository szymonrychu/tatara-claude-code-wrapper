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

func TestStore_TouchUpdatesRunningTurn(t *testing.T) {
	s := turn.NewStore()
	t0 := time.Unix(1000, 0)
	rec := s.Create("turn-1", "hello", "", t0)
	require.Equal(t, t0, rec.LastActivityAt, "Create defaults LastActivityAt to StartedAt")

	t1 := time.Unix(1007, 0)
	require.True(t, s.Touch("turn-1", t1))

	got, _ := s.Get("turn-1")
	require.Equal(t, t1, got.LastActivityAt)
	require.Equal(t, t0, got.StartedAt)
}

func TestStore_TouchNoopOnUnknownOrTerminal(t *testing.T) {
	s := turn.NewStore()
	require.False(t, s.Touch("missing", time.Unix(1, 0)))

	t0 := time.Unix(1000, 0)
	s.Create("done", "x", "", t0)
	require.NoError(t, s.Complete("done", "ok", nil, nil, "end_turn", time.Unix(1005, 0)))
	require.False(t, s.Touch("done", time.Unix(2000, 0)))
	got, _ := s.Get("done")
	require.Equal(t, t0, got.LastActivityAt, "Touch must not mutate a completed turn")

	s.Create("boom", "y", "", t0)
	require.NoError(t, s.Fail("boom", "kaboom", time.Unix(1005, 0)))
	require.False(t, s.Touch("boom", time.Unix(2000, 0)))
	got, _ = s.Get("boom")
	require.Equal(t, t0, got.LastActivityAt, "Touch must not mutate a failed turn")
}

func TestStore_ListCarriesLastActivityAt(t *testing.T) {
	s := turn.NewStore()
	t0 := time.Unix(1000, 0)
	s.Create("turn-1", "hello", "", t0)
	require.True(t, s.Touch("turn-1", time.Unix(1009, 0)))

	list := s.List()
	require.Len(t, list, 1)
	require.Equal(t, time.Unix(1009, 0), list[0].LastActivityAt)
}
