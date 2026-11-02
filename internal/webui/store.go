package webui

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/alvarorichard/Goanime/internal/models"
)

// Entry is everything the UI remembers about one anime: enough of the scraped
// model to list its episodes again after a restart, plus favorite and
// watch-progress state.
type Entry struct {
	ID          string        `json:"id"`
	Anime       *models.Anime `json:"anime"`
	Favorite    bool          `json:"favorite"`
	FavoritedAt time.Time     `json:"favoritedAt,omitzero"`
	Episodes    []string      `json:"episodes,omitempty"` // episode labels, in list order
	Watched     []string      `json:"watched,omitempty"`
	LastEp      string        `json:"lastEp,omitempty"`
	Position    float64       `json:"position,omitempty"`
	Duration    float64       `json:"duration,omitempty"`
	WatchedAt   time.Time     `json:"watchedAt,omitzero"`
}

// finished reports whether the last episode played reached its end credits.
func (e *Entry) finished() bool {
	return e.Duration > 0 && e.Position/e.Duration >= watchedRatio
}

// nextEpisode is the episode "Continuar" should play: the last one again when
// it was left midway, else the one after it.
func (e *Entry) nextEpisode() string {
	if e.LastEp == "" || !e.finished() {
		return e.LastEp
	}
	i := slices.Index(e.Episodes, e.LastEp)
	if i >= 0 && i+1 < len(e.Episodes) {
		return e.Episodes[i+1]
	}
	return e.LastEp
}

// watchedRatio is how far into an episode counts as having watched it.
const watchedRatio = 0.9

// Store persists entries as one JSON file under the GoAnime data directory.
type Store struct {
	mu      sync.Mutex
	path    string
	entries map[string]*Entry
}

func OpenStore(path string) (*Store, error) {
	s := &Store{path: path, entries: map[string]*Entry{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &s.entries); err != nil {
		return nil, err
	}
	return s, nil
}

// saveLocked writes the file atomically so a crash never leaves it truncated.
func (s *Store) saveLocked() error {
	data, err := json.MarshalIndent(s.entries, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Get returns a copy of the entry, or nil.
func (s *Store) Get(id string) *Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[id]
	if !ok {
		return nil
	}
	c := *e
	c.Episodes = slices.Clone(e.Episodes)
	c.Watched = slices.Clone(e.Watched)
	return &c
}

// Update applies fn to the entry for anime (creating it) and saves.
func (s *Store) Update(id string, anime *models.Anime, fn func(*Entry)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[id]
	if !ok {
		e = &Entry{ID: id}
		s.entries[id] = e
	}
	if anime != nil {
		e.Anime = anime
	}
	fn(e)
	if !e.Favorite && e.LastEp == "" {
		delete(s.entries, id)
	}
	return s.saveLocked()
}

// List returns copies of the entries matching keep, sorted by less.
func (s *Store) List(keep func(*Entry) bool, less func(a, b *Entry) bool) []*Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Entry
	for _, e := range s.entries {
		if keep(e) {
			c := *e
			out = append(out, &c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return less(out[i], out[j]) })
	return out
}
