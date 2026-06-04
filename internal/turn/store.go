package turn

import (
	"encoding/json"
	"errors"
	"sync"
	"time"
)

var ErrNotFound = errors.New("turn: not found")

type Store struct {
	mu    sync.RWMutex
	byID  map[string]*Record
	order []string
}

func NewStore() *Store { return &Store{byID: map[string]*Record{}} }

func (s *Store) Create(id, text, callbackURL string, now time.Time) *Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := &Record{ID: id, State: Running, Text: text, CallbackURL: callbackURL, StartedAt: now}
	s.byID[id] = rec
	s.order = append(s.order, id)
	return rec
}

func (s *Store) Get(id string) (*Record, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.byID[id]
	if !ok {
		return nil, false
	}
	cp := *rec
	return &cp, true
}

func (s *Store) List() []Summary {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Summary, 0, len(s.order))
	for _, id := range s.order {
		r := s.byID[id]
		out = append(out, Summary{ID: r.ID, State: r.State, StartedAt: r.StartedAt, CompletedAt: r.CompletedAt})
	}
	return out
}

func (s *Store) Complete(id, finalText string, resultJSON, usage json.RawMessage, stopReason string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.byID[id]
	if !ok {
		return ErrNotFound
	}
	r.State, r.FinalText, r.ResultJSON, r.Usage, r.StopReason = Complete, finalText, resultJSON, usage, stopReason
	r.CompletedAt = &now
	return nil
}

func (s *Store) Fail(id, msg string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.byID[id]
	if !ok {
		return ErrNotFound
	}
	r.State, r.Error, r.CompletedAt = Failed, msg, &now
	return nil
}
