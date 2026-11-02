package webui

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/alvarorichard/Goanime/internal/api/providers"
	"github.com/alvarorichard/Goanime/internal/api/source"
	"github.com/alvarorichard/Goanime/internal/models"
)

// sourceOption is a source the UI can filter by: the anime sources. StartFlix
// and TopCine (movie/TV catalogs with a season picker) are left out, and
// Goyabu is parked upstream behind a Cloudflare gate.
type sourceOption struct {
	Key   string            `json:"key"`
	Label string            `json:"label"`
	Kind  source.SourceKind `json:"-"`
}

var sourceOptions = []sourceOption{
	{Key: "animefire", Label: "AnimeFire", Kind: source.AnimeFire},
	{Key: "hianime", Label: "HiAnime", Kind: source.HiAnime},
}

func sourceKinds(key string) ([]source.SourceKind, error) {
	if key == "" || key == "todas" {
		kinds := make([]source.SourceKind, len(sourceOptions))
		for i, o := range sourceOptions {
			kinds[i] = o.Kind
		}
		return kinds, nil
	}
	for _, o := range sourceOptions {
		if o.Key == key {
			return []source.SourceKind{o.Kind}, nil
		}
	}
	return nil, fmt.Errorf("fonte desconhecida: %s", key)
}

// sourceLabel turns the Source field the scrapers stamp ("Animefire.io", …)
// into the label the UI shows.
func sourceLabel(s string) string {
	l := strings.ToLower(s)
	for _, o := range sourceOptions {
		if strings.Contains(l, o.Key) {
			return o.Label
		}
	}
	return s
}

func animeID(a *models.Anime) string {
	sum := sha1.Sum([]byte(a.Source + "|" + a.URL))
	return hex.EncodeToString(sum[:6])
}

var leadingTagRe = regexp.MustCompile(`^\s*\[([^\]]+)\]\s*`)

// splitTitle separates the "[PT-BR] [Movie]" tags tagResults prefixes onto a
// name from the title itself.
func splitTitle(name string) (title string, tags []string) {
	for {
		m := leadingTagRe.FindStringSubmatchIndex(name)
		if m == nil {
			return strings.TrimSpace(name), tags
		}
		tags = append(tags, name[m[2]:m[3]])
		name = name[m[1]:]
	}
}

func episodeLabel(ep *models.Episode) string {
	n := strings.TrimSpace(providers.EpisodeNumber(ep))
	if n == "" {
		n = strconv.Itoa(ep.Num)
	}
	if i, err := strconv.Atoi(n); err == nil {
		return fmt.Sprintf("%02d", i)
	}
	return n
}

// Catalog caches what the scrapers returned during this run, so the UI can
// refer to animes and episodes by short ids.
type Catalog struct {
	store *Store

	mu       sync.Mutex
	animes   map[string]*models.Anime
	episodes map[string][]models.Episode
}

func NewCatalog(store *Store) *Catalog {
	return &Catalog{store: store, animes: map[string]*models.Anime{}, episodes: map[string][]models.Episode{}}
}

func (c *Catalog) Search(ctx context.Context, query, sourceKey string) ([]*models.Anime, error) {
	kinds, err := sourceKinds(sourceKey)
	if err != nil {
		return nil, err
	}
	results, err := providers.SearchAll(ctx, query, kinds...)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	for _, a := range results {
		c.animes[animeID(a)] = a
	}
	c.mu.Unlock()
	return results, nil
}

// Anime finds an anime from this run's searches or, after a restart, from the
// favorites/history store.
func (c *Catalog) Anime(id string) (*models.Anime, bool) {
	c.mu.Lock()
	a, ok := c.animes[id]
	c.mu.Unlock()
	if ok {
		return a, true
	}
	if e := c.store.Get(id); e != nil && e.Anime != nil {
		c.mu.Lock()
		c.animes[id] = e.Anime
		c.mu.Unlock()
		return e.Anime, true
	}
	return nil, false
}

func (c *Catalog) Episodes(ctx context.Context, id string) ([]models.Episode, error) {
	c.mu.Lock()
	eps, ok := c.episodes[id]
	c.mu.Unlock()
	if ok {
		return eps, nil
	}
	a, ok := c.Anime(id)
	if !ok {
		return nil, errNotFound
	}
	eps, err := providers.FetchEpisodes(ctx, a)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.episodes[id] = eps
	c.mu.Unlock()

	// Keep the stored label list current so "Continuar" can find the next
	// episode without scraping again.
	if c.store.Get(id) != nil {
		labels := episodeLabels(eps)
		_ = c.store.Update(id, nil, func(e *Entry) { e.Episodes = labels })
	}
	return eps, nil
}

func (c *Catalog) Episode(ctx context.Context, id, label string) (*models.Anime, *models.Episode, error) {
	a, ok := c.Anime(id)
	if !ok {
		return nil, nil, errNotFound
	}
	eps, err := c.Episodes(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	for i := range eps {
		if episodeLabel(&eps[i]) == label {
			return a, &eps[i], nil
		}
	}
	return nil, nil, fmt.Errorf("episódio %s não encontrado", label)
}

func episodeLabels(eps []models.Episode) []string {
	labels := make([]string, len(eps))
	for i := range eps {
		labels[i] = episodeLabel(&eps[i])
	}
	return labels
}
