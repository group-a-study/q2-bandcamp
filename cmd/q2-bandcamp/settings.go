package main

import (
	"encoding/json"
	"os"
	"sync"

	"github.com/group-a-study/q2-bandcamp/internal/q2"
)

// Settings, kept in .bandcamp/settings.json: how many of New and Notable's and of Album of the Day's
// newest releases their Download the Top N downloads, each 10 until chosen.

const defaultTop = 10

// topChoices are the Ns the How Many page offers (the lists hold 120).
var topChoices = []int{1, 3, 5, 10, 15, 20, 25, 30, 40, 50, 75, 100, 120}

type settings struct {
	path string
	mu   sync.Mutex
	Top  map[string]int `json:"top"` // by list: nn, aotd
}

func readSettings(path string) *settings {
	s := &settings{path: path}
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, s)
	}
	if s.Top == nil {
		s.Top = map[string]int{}
	}
	return s
}

// top is the list's N.
func (s *settings) top(list string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n := s.Top[list]; n >= 1 && n <= 120 {
		return n
	}
	return defaultTop
}

func (s *settings) setTop(list string, n int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, had := s.Top[list]
	s.Top[list] = n
	b, _ := json.Marshal(s)
	if err := q2.WriteFile(s.path, b); err != nil {
		if had {
			s.Top[list] = old
		} else {
			delete(s.Top, list)
		}
		return err
	}
	return nil
}
