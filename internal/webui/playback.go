package webui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/alvarorichard/Goanime/internal/api/providers"
	"github.com/alvarorichard/Goanime/internal/models"
	"github.com/alvarorichard/Goanime/internal/player"
	"github.com/alvarorichard/Goanime/internal/util"
)

// resolveMu serializes stream resolution. The scrapers report the referer and
// user agent a stream needs through process-wide globals, so a resolution must
// be consumed before the next one starts.
var resolveMu sync.Mutex

// PlayerStatus is what the sidebar shows about mpv.
type PlayerStatus struct {
	State    string     `json:"state"` // idle | resolving | playing | error
	AnimeID  string     `json:"animeId,omitempty"`
	Title    string     `json:"title,omitempty"`
	Episode  string     `json:"episode,omitempty"`
	Position float64    `json:"position,omitempty"`
	Duration float64    `json:"duration,omitempty"`
	Error    string     `json:"error,omitempty"`
	Subs     []SubTrack `json:"subs,omitempty"`
	Sid      int        `json:"sid"` // selected subtitle track; 0 = none
}

// SubTrack is one subtitle track mpv has loaded.
type SubTrack struct {
	ID    int    `json:"id"`
	Label string `json:"label"`
}

type Player struct {
	catalog *Catalog
	store   *Store

	mu     sync.Mutex
	status PlayerStatus
	socket string
	subs   []util.SubtitleInfo // tracks the source resolved, for their labels
	gen    int                 // bumped per playback so a stale watcher stops updating status
}

func NewPlayer(c *Catalog, s *Store) *Player {
	return &Player{catalog: c, store: s, status: PlayerStatus{State: "idle"}}
}

func (p *Player) Status() PlayerStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status
}

// Play resolves and starts an episode in the background; progress shows up in
// Status. A playback already running is closed first. subLang is the
// subtitle preference: "pt", "en" or "off".
func (p *Player) Play(id, label, quality, subLang string) error {
	a, ok := p.catalog.Anime(id)
	if !ok {
		return errNotFound
	}
	title, _ := splitTitle(a.Name)

	p.mu.Lock()
	prev, prevSocket := p.status, p.socket
	p.socket = ""
	p.subs = nil
	p.gen++
	gen := p.gen
	p.status = PlayerStatus{State: "resolving", AnimeID: id, Title: title, Episode: label}
	p.mu.Unlock()

	// The previous watcher stops recording once gen moves on, so save where
	// that episode was left before closing it.
	if prevSocket != "" {
		if prevAnime, ok := p.catalog.Anime(prev.AnimeID); ok {
			p.record(prev.AnimeID, prevAnime, prev.Episode, prev.Position, prev.Duration)
		}
		_, _ = player.MpvSendCommand(prevSocket, []any{"quit"})
	}

	go func() {
		if err := p.run(gen, id, label, quality, subLang, title); err != nil {
			p.setStatus(gen, func(s *PlayerStatus) {
				s.State = "error"
				s.Error = err.Error()
			})
		}
	}()
	return nil
}

func (p *Player) setStatus(gen int, fn func(*PlayerStatus)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if gen == p.gen {
		fn(&p.status)
	}
}

func (p *Player) run(gen int, id, label, quality, subLang, title string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	a, ep, err := p.catalog.Episode(ctx, id, label)
	if err != nil {
		return err
	}

	resume := 0
	if e := p.store.Get(id); e != nil && e.LastEp == label && !e.finished() && e.Position > 30 {
		resume = int(e.Position) - 5
	}

	resolveMu.Lock()
	streamURL, err := providers.FetchStreamURL(ctx, ep, a, quality)
	if err != nil {
		resolveMu.Unlock()
		return fmt.Errorf("não foi possível obter o vídeo: %w", err)
	}
	subs := util.GetGlobalSubtitles()
	wp := player.PrepareWebPlayback(streamURL, fmt.Sprintf("%s · EP %s", title, label), resume, subs, subLang)
	socket, err := player.StartVideo(wp.URL, wp.Args)
	resolveMu.Unlock()
	if err != nil {
		return fmt.Errorf("o mpv não abriu: %w", err)
	}
	defer player.StopBloggerProxy()

	p.mu.Lock()
	if gen != p.gen {
		// Another episode was requested while this one resolved.
		p.mu.Unlock()
		_, _ = player.MpvSendCommand(socket, []any{"quit"})
		return nil
	}
	p.socket = socket
	p.subs = subs
	p.status.State = "playing"
	p.mu.Unlock()

	if len(wp.LaterSubs) > 0 {
		go player.AddSubtitlesOncePlaying(socket, wp.LaterSubs)
	}

	if wp.IsHLS && resume > 0 && player.WaitForVideoReady(socket) {
		_, _ = player.MpvSendCommand(socket, []any{"seek", resume, "absolute"})
	}

	p.record(id, a, label, float64(resume), 0)
	p.watch(gen, socket, id, a, label)
	return nil
}

// watch polls mpv until it exits, saving progress along the way. Only a
// socket that stops answering counts as mpv closing: while a stream loads
// time-pos is merely unavailable.
func (p *Player) watch(gen int, socket, id string, a *models.Anime, label string) {
	var pos, dur float64
	failures := 0
	lastSave := time.Now()
	for failures < 3 {
		time.Sleep(2 * time.Second)
		v, err := player.MpvGet(socket, "time-pos")
		if errors.Is(err, player.ErrPropertyUnavailable) {
			failures = 0
			continue
		}
		if err != nil {
			failures++
			continue
		}
		failures = 0
		if f, ok := v.(float64); ok {
			pos = f
		}
		if v, err := player.MpvGet(socket, "duration"); err == nil {
			if f, ok := v.(float64); ok {
				dur = f
			}
		}
		subs, sid := p.subTracks(socket)
		p.setStatus(gen, func(s *PlayerStatus) { s.Position, s.Duration, s.Subs, s.Sid = pos, dur, subs, sid })
		if !p.current(gen) {
			return
		}
		if time.Since(lastSave) > 15*time.Second {
			p.record(id, a, label, pos, dur)
			lastSave = time.Now()
		}
	}
	if !p.current(gen) {
		return
	}
	p.record(id, a, label, pos, dur)
	p.setStatus(gen, func(s *PlayerStatus) { *s = PlayerStatus{State: "idle"} })
	p.mu.Lock()
	if gen == p.gen {
		p.socket = ""
	}
	p.mu.Unlock()
}

// subTracks lists the subtitle tracks mpv has loaded and the selected one.
// External tracks are named after the label the source gave them; mpv only
// knows their URL.
func (p *Player) subTracks(socket string) ([]SubTrack, int) {
	v, err := player.MpvGet(socket, "track-list")
	if err != nil {
		return nil, 0
	}
	p.mu.Lock()
	known := p.subs
	p.mu.Unlock()
	return parseSubTracks(v, known)
}

func parseSubTracks(trackList any, known []util.SubtitleInfo) ([]SubTrack, int) {
	list, _ := trackList.([]any)
	var tracks []SubTrack
	sid := 0
	for _, item := range list {
		t, _ := item.(map[string]any)
		if t["type"] != "sub" {
			continue
		}
		id, _ := t["id"].(float64)
		label := ""
		if file, _ := t["external-filename"].(string); file != "" {
			for _, k := range known {
				if k.URL == file {
					label = k.Label
					break
				}
			}
		}
		for _, key := range []string{"title", "lang"} {
			if label == "" {
				label, _ = t[key].(string)
			}
		}
		if label == "" {
			label = fmt.Sprintf("Faixa %d", int(id))
		}
		tracks = append(tracks, SubTrack{ID: int(id), Label: label})
		if selected, _ := t["selected"].(bool); selected {
			sid = int(id)
		}
	}
	return tracks, sid
}

// SetSubtitle switches the playing episode to subtitle track sid, or turns
// subtitles off when sid is 0.
func (p *Player) SetSubtitle(sid int) error {
	p.mu.Lock()
	socket := p.socket
	p.mu.Unlock()
	if socket == "" {
		return errNotPlaying
	}
	var value any = sid
	if sid == 0 {
		value = "no"
	}
	if _, err := player.MpvSendCommand(socket, []any{"set_property", "sid", value}); err != nil {
		return fmt.Errorf("o mpv não trocou a legenda: %w", err)
	}
	p.mu.Lock()
	if p.socket == socket {
		p.status.Sid = sid
	}
	p.mu.Unlock()
	return nil
}

func (p *Player) current(gen int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return gen == p.gen
}

func (p *Player) record(id string, a *models.Anime, label string, pos, dur float64) {
	var labels []string
	if eps, err := p.catalog.Episodes(context.Background(), id); err == nil {
		labels = episodeLabels(eps)
	}
	_ = p.store.Update(id, a, func(e *Entry) {
		if labels != nil {
			e.Episodes = labels
		}
		if e.LastEp != label {
			e.Position, e.Duration = 0, 0
		}
		e.LastEp = label
		e.WatchedAt = time.Now()
		if pos > 0 || dur > 0 {
			e.Position, e.Duration = pos, dur
		}
		if e.finished() && !slices.Contains(e.Watched, label) {
			e.Watched = append(e.Watched, label)
		}
	})
}
