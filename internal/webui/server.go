// Package webui serves GoAnime's browser interface: a local HTTP server whose
// API drives the same scrapers and mpv setup the CLI uses.
package webui

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/alvarorichard/Goanime/internal/player"
)

//go:embed static
var staticFiles embed.FS

var (
	errNotFound   = errors.New("anime não encontrado — faça a busca novamente")
	errNotPlaying = errors.New("nenhum episódio tocando")
)

type Server struct {
	catalog   *Catalog
	store     *Store
	player    *Player
	downloads *Downloads
	imgClient *http.Client
}

func NewServer(store *Store, downloadDir string) *Server {
	c := NewCatalog(store)
	return &Server{
		catalog:   c,
		store:     store,
		player:    NewPlayer(c, store),
		downloads: NewDownloads(c, downloadDir),
		imgClient: &http.Client{Timeout: 15 * time.Second},
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(staticFiles, "static")
	mux.Handle("GET /", http.FileServerFS(static))

	mux.HandleFunc("GET /api/sources", s.handleSources)
	mux.HandleFunc("GET /api/search", s.handleSearch)
	mux.HandleFunc("GET /api/home", s.handleHome)
	mux.HandleFunc("GET /api/anime/{id}", s.handleAnime)
	mux.HandleFunc("POST /api/anime/{id}/favorite", s.handleFavorite)
	mux.HandleFunc("DELETE /api/anime/{id}/history", s.handleForget)
	mux.HandleFunc("POST /api/play", s.handlePlay)
	mux.HandleFunc("GET /api/player", s.handlePlayer)
	mux.HandleFunc("POST /api/player/subtitle", s.handleSubtitle)
	mux.HandleFunc("GET /api/downloads", s.handleDownloads)
	mux.HandleFunc("POST /api/downloads", s.handleEnqueue)
	mux.HandleFunc("DELETE /api/downloads/{job}", s.handleCancel)
	mux.HandleFunc("POST /api/downloads/play", s.handlePlayFile)
	mux.HandleFunc("POST /api/downloads/open", s.handleOpenFolder)
	mux.HandleFunc("GET /api/img", s.handleImage)
	return localOnly(mux)
}

// localOnly rejects requests that did not come from the UI itself: a Host
// other than localhost (DNS rebinding) or a cross-site Origin on a state
// change (another tab POSTing to the API).
func localOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if host != "127.0.0.1" && host != "localhost" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet {
			if o := r.Header.Get("Origin"); o != "" {
				u, err := url.Parse(o)
				if err != nil || u.Host != r.Host {
					http.Error(w, "forbidden", http.StatusForbidden)
					return
				}
			}
			if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
				http.Error(w, "expected JSON", http.StatusUnsupportedMediaType)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	code := http.StatusBadGateway
	switch {
	case errors.Is(err, errNotFound):
		code = http.StatusNotFound
	case errors.Is(err, errNotPlaying):
		code = http.StatusConflict
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func readJSON(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(v)
}

// animeCard is the summary the grids render.
type animeCard struct {
	ID       string   `json:"id"`
	Title    string   `json:"title"`
	Tags     []string `json:"tags,omitempty"`
	Source   string   `json:"source"`
	Image    string   `json:"image,omitempty"`
	Episodes int      `json:"episodes,omitempty"`
}

func (s *Server) card(id string, name, src, image string) animeCard {
	title, tags := splitTitle(name)
	return animeCard{ID: id, Title: title, Tags: tags, Source: sourceLabel(src), Image: image}
}

func (s *Server) handleSources(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, sourceOptions)
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, []animeCard{})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	results, err := s.catalog.Search(ctx, q, r.URL.Query().Get("source"))
	if err != nil {
		// "No results" is an empty list, not a failure.
		if strings.HasPrefix(err.Error(), "no results found") {
			writeJSON(w, []animeCard{})
			return
		}
		writeErr(w, err)
		return
	}
	cards := make([]animeCard, 0, len(results))
	for _, a := range results {
		cards = append(cards, s.card(animeID(a), a.Name, a.Source, a.ImageURL))
	}
	writeJSON(w, cards)
}

type continueCard struct {
	animeCard
	Episode  string  `json:"episode"`
	Position float64 `json:"position"`
	Duration float64 `json:"duration"`
	Next     string  `json:"next"`
}

func (s *Server) handleHome(w http.ResponseWriter, _ *http.Request) {
	history := s.store.List(
		func(e *Entry) bool { return e.LastEp != "" && e.Anime != nil },
		func(a, b *Entry) bool { return a.WatchedAt.After(b.WatchedAt) },
	)
	favorites := s.store.List(
		func(e *Entry) bool { return e.Favorite && e.Anime != nil },
		func(a, b *Entry) bool { return a.FavoritedAt.After(b.FavoritedAt) },
	)
	cont := make([]continueCard, 0, len(history))
	for _, e := range history {
		c := continueCard{animeCard: s.card(e.ID, e.Anime.Name, e.Anime.Source, e.Anime.ImageURL), Episode: e.LastEp, Next: e.nextEpisode()}
		c.Episodes = len(e.Episodes)
		if c.Next == e.LastEp {
			c.Position, c.Duration = e.Position, e.Duration
		}
		cont = append(cont, c)
	}
	favs := make([]animeCard, 0, len(favorites))
	for _, e := range favorites {
		c := s.card(e.ID, e.Anime.Name, e.Anime.Source, e.Anime.ImageURL)
		c.Episodes = len(e.Episodes)
		favs = append(favs, c)
	}
	writeJSON(w, map[string]any{"continue": cont, "favorites": favs})
}

type episodeView struct {
	Label   string `json:"label"`
	Title   string `json:"title,omitempty"`
	Filler  bool   `json:"filler,omitempty"`
	Watched bool   `json:"watched,omitempty"`
}

func (s *Server) handleAnime(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, ok := s.catalog.Anime(id)
	if !ok {
		writeErr(w, errNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	eps, err := s.catalog.Episodes(ctx, id)
	if err != nil {
		writeErr(w, err)
		return
	}
	e := s.store.Get(id)
	if e == nil {
		e = &Entry{}
	}
	views := make([]episodeView, len(eps))
	for i := range eps {
		l := episodeLabel(&eps[i])
		t := eps[i].Title.English
		if t == "" {
			t = eps[i].Title.Romaji
		}
		views[i] = episodeView{Label: l, Title: t, Filler: eps[i].IsFiller, Watched: slices.Contains(e.Watched, l)}
	}
	c := s.card(id, a.Name, a.Source, a.ImageURL)
	c.Episodes = len(eps)
	desc := a.Details.Description
	if desc == "" {
		desc = a.Overview
	}
	writeJSON(w, map[string]any{
		"anime":       c,
		"description": desc,
		"genres":      a.Details.Genres,
		"episodes":    views,
		"favorite":    e.Favorite,
		"lastEp":      e.LastEp,
		"next":        e.nextEpisode(),
		"position":    e.Position,
		"duration":    e.Duration,
	})
}

func (s *Server) handleFavorite(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Favorite bool `json:"favorite"`
	}
	if err := readJSON(r, &body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a, ok := s.catalog.Anime(id)
	if !ok {
		writeErr(w, errNotFound)
		return
	}
	var labels []string
	if eps, err := s.catalog.Episodes(r.Context(), id); err == nil {
		labels = episodeLabels(eps)
	}
	err := s.store.Update(id, a, func(e *Entry) {
		e.Favorite = body.Favorite
		e.FavoritedAt = time.Now()
		if labels != nil {
			e.Episodes = labels
		}
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]bool{"favorite": body.Favorite})
}

func (s *Server) handleForget(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := s.store.Update(id, nil, func(e *Entry) {
		e.LastEp, e.Position, e.Duration, e.Watched = "", 0, 0, nil
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePlay(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID      string `json:"id"`
		Episode string `json:"episode"`
		Quality string `json:"quality"`
		Subs    string `json:"subs"` // "pt", "en" or "off"
	}
	if err := readJSON(r, &body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.player.Play(body.ID, body.Episode, body.Quality, body.Subs); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handlePlayer(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.player.Status())
}

func (s *Server) handleSubtitle(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Sid int `json:"sid"` // 0 = off
	}
	if err := readJSON(r, &body); err != nil || body.Sid < 0 {
		http.Error(w, "faixa inválida", http.StatusBadRequest)
		return
	}
	if err := s.player.SetSubtitle(body.Sid); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDownloads(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"dir":   s.downloads.Dir(),
		"jobs":  s.downloads.Jobs(),
		"files": s.downloads.Files(),
	})
}

func (s *Server) handleEnqueue(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID       string   `json:"id"`
		Episodes []string `json:"episodes"`
		Quality  string   `json:"quality"`
	}
	if err := readJSON(r, &body); err != nil || len(body.Episodes) == 0 {
		http.Error(w, "pedido inválido", http.StatusBadRequest)
		return
	}
	if err := s.downloads.Enqueue(body.ID, body.Episodes, body.Quality); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("job"))
	if err != nil {
		http.Error(w, "job inválido", http.StatusBadRequest)
		return
	}
	s.downloads.Cancel(id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePlayFile(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	if err := readJSON(r, &body); err != nil || !s.downloads.Contains(body.Path) {
		http.Error(w, "arquivo inválido", http.StatusBadRequest)
		return
	}
	if _, err := player.StartVideo(body.Path, nil); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleOpenFolder(w http.ResponseWriter, _ *http.Request) {
	if err := exec.Command("xdg-open", s.downloads.Dir()).Start(); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleImage proxies cover art so hotlink-protected hosts still serve it.
func (s *Server) handleImage(w http.ResponseWriter, r *http.Request) {
	u, err := url.Parse(r.URL.Query().Get("u"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		http.Error(w, "bad url", http.StatusBadRequest)
		return
	}
	if ip := net.ParseIP(u.Hostname()); u.Hostname() == "localhost" || (ip != nil && (ip.IsLoopback() || ip.IsPrivate())) {
		http.Error(w, "bad url", http.StatusBadRequest)
		return
	}
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, u.String(), nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0 Safari/537.36")
	req.Header.Set("Referer", u.Scheme+"://"+u.Host+"/")
	resp, err := s.imgClient.Do(req)
	if err != nil {
		http.Error(w, "upstream", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	ct := resp.Header.Get("Content-Type")
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(ct, "image/") {
		http.Error(w, "upstream", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = io.Copy(w, io.LimitReader(resp.Body, 10<<20))
}
