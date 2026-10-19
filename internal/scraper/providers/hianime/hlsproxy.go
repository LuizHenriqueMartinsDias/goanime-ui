package hianime

import (
	"bufio"
	"bytes"
	"container/list"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alvarorichard/Goanime/internal/scraper/netx"
	"github.com/alvarorichard/Goanime/internal/util"
)

// hlsProxy is a loopback HTTP server that hands mpv and the downloader a plain
// HLS stream for a MegaPlay episode.
//
// Two things stop the upstream stream from being played directly: every request
// needs the embed's Referer, and each segment is MPEG-TS with a whole PNG image
// (signature, IHDR, IDAT, IEND) glued in front, so ffmpeg probes it as a
// picture. The proxy rewrites each playlist so that every URI points back at
// itself, then serves segments with the PNG cut off.
//
// Only URLs that appeared in a playlist it rewrote are fetchable: each one is
// registered under a random id, so the listener is not an open proxy for other
// local processes.
type hlsProxy struct {
	mu      sync.Mutex
	base    string                   // http://127.0.0.1:port
	entries map[string]*list.Element // id -> element of lru
	byURL   map[string]string        // upstream+referer -> id, so a re-read playlist reuses ids
	lru     *list.List               // front = most recently used; values are *hlsEntry
}

type hlsEntry struct {
	id        string
	upstream  string
	referer   string
	userAgent string
	playlist  bool
	client    *http.Client
}

// maxHLSEntries bounds the registry. An episode registers a few hundred
// segments, so this holds a long batch download while a few hundred bytes per
// entry keep it small.
const maxHLSEntries = 100_000

// maxPlaylistBytes caps a playlist read; a 24-minute episode's is ~70KB.
const maxPlaylistBytes = 8 << 20

// maxPNGChunk caps one chunk of the PNG wrapper. The wrappers seen are a 1×1
// image of 70 bytes; anything this large is not a wrapper.
const maxPNGChunk = 16 << 20

var pngSignature = []byte("\x89PNG\r\n\x1a\n")

// uriAttrRe matches the URI="…" attribute of tags such as EXT-X-KEY.
var uriAttrRe = regexp.MustCompile(`URI="([^"]+)"`)

var (
	hlsProxyOnce   sync.Once
	hlsProxyShared *hlsProxy
)

func newHLSProxy() *hlsProxy {
	return &hlsProxy{entries: map[string]*list.Element{}, byURL: map[string]string{}, lru: list.New()}
}

func sharedHLSProxy() *hlsProxy {
	hlsProxyOnce.Do(func() { hlsProxyShared = newHLSProxy() })
	return hlsProxyShared
}

// newStreamClient is the client the proxy uses upstream: SSRF-guarded like
// every scraper request, with no whole-request timeout because a segment is
// read for as long as the network takes.
func newStreamClient() *http.Client {
	t := netx.SafeScraperTransport(30 * time.Second)
	t.ResponseHeaderTimeout = 30 * time.Second
	return &http.Client{Transport: t}
}

// serve registers a playlist and returns its local URL, starting the listener
// on first use.
func (p *hlsProxy) serve(playlistURL, referer, userAgent string, client *http.Client) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.base == "" {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return "", fmt.Errorf("local HLS proxy: %w", err)
		}
		srv := &http.Server{Handler: p, ReadHeaderTimeout: 10 * time.Second}
		go func() {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				util.Debug("HiAnime HLS proxy stopped", "err", err)
			}
		}()
		p.base = "http://" + ln.Addr().String()
		util.RegisterLocalProxyHost(ln.Addr().String())
		util.Debug("HiAnime HLS proxy listening", "addr", p.base)
	}
	return p.registerLocked(&hlsEntry{
		upstream: playlistURL, referer: referer, userAgent: userAgent, playlist: true, client: client,
	})
}

// registerLocked stores e (or finds the entry already registered for the same
// URL) and returns its local URL. The extension tells ffmpeg's HLS demuxer
// what each URL is.
func (p *hlsProxy) registerLocked(e *hlsEntry) (string, error) {
	key := e.upstream + "\x00" + e.referer
	if id, ok := p.byURL[key]; ok {
		if el, ok := p.entries[id]; ok {
			p.lru.MoveToFront(el)
			return p.localURL(el.Value.(*hlsEntry)), nil
		}
	}

	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	e.id = hex.EncodeToString(raw[:])
	p.entries[e.id] = p.lru.PushFront(e)
	p.byURL[key] = e.id
	for p.lru.Len() > maxHLSEntries {
		oldest := p.lru.Back()
		p.lru.Remove(oldest)
		old := oldest.Value.(*hlsEntry)
		delete(p.entries, old.id)
		delete(p.byURL, old.upstream+"\x00"+old.referer)
	}
	return p.localURL(e), nil
}

func (p *hlsProxy) localURL(e *hlsEntry) string {
	if e.playlist {
		return p.base + "/hianime/" + e.id + ".m3u8"
	}
	return p.base + "/hianime/" + e.id + ".ts"
}

// lookup finds a registered entry and marks it most recently used.
func (p *hlsProxy) lookup(path string) (*hlsEntry, bool) {
	name := strings.TrimPrefix(path, "/hianime/")
	id := strings.TrimSuffix(strings.TrimSuffix(name, ".m3u8"), ".ts")
	p.mu.Lock()
	defer p.mu.Unlock()
	el, ok := p.entries[id]
	if !ok {
		return nil, false
	}
	p.lru.MoveToFront(el)
	return el.Value.(*hlsEntry), true
}

func (p *hlsProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	e, ok := p.lookup(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, e.upstream, http.NoBody)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	req.Header.Set("User-Agent", e.userAgent)
	req.Header.Set("Referer", e.referer)
	resp, err := e.client.Do(req) // #nosec G704 -- only URLs from a playlist this proxy rewrote
	if err != nil {
		util.Debug("HiAnime HLS proxy upstream failed", "err", err)
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		util.Debug("HiAnime HLS proxy upstream refused", "status", resp.StatusCode)
		http.Error(w, "upstream "+resp.Status, http.StatusBadGateway)
		return
	}

	if e.playlist {
		p.servePlaylist(w, r, e, resp.Body)
		return
	}
	serveSegment(w, r, resp)
}

func (p *hlsProxy) servePlaylist(w http.ResponseWriter, r *http.Request, e *hlsEntry, body io.Reader) {
	raw, err := io.ReadAll(io.LimitReader(body, maxPlaylistBytes))
	if err != nil {
		http.Error(w, "upstream read failed", http.StatusBadGateway)
		return
	}
	out, err := p.rewritePlaylist(e, string(raw))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Content-Length", strconv.Itoa(len(out)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = io.WriteString(w, out)
	}
}

// rewritePlaylist points every URI of a playlist at the proxy. Nested
// playlists (variants, renditions) stay playlists; everything else is served
// as a segment. I-frame playlists are dropped: their byte ranges index the
// wrapped files, and no player of ours reads them.
func (p *hlsProxy) rewritePlaylist(parent *hlsEntry, body string) (string, error) {
	if !strings.HasPrefix(strings.TrimSpace(body), "#EXTM3U") {
		return "", errors.New("upstream answered with no playlist")
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	register := func(ref string, playlist bool) (string, error) {
		abs := resolveRef(parent.upstream, ref)
		return p.registerLocked(&hlsEntry{
			upstream: abs, referer: parent.referer, userAgent: parent.userAgent,
			playlist: playlist || isPlaylistURL(abs), client: parent.client,
		})
	}

	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	out := make([]string, 0, len(lines))
	nextIsVariant := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
			out = append(out, line)
		case strings.HasPrefix(trimmed, "#EXT-X-I-FRAME-STREAM-INF"):
			continue
		case strings.HasPrefix(trimmed, "#"):
			nested := strings.HasPrefix(trimmed, "#EXT-X-MEDIA:")
			var regErr error
			line = uriAttrRe.ReplaceAllStringFunc(line, func(attr string) string {
				local, err := register(uriAttrRe.FindStringSubmatch(attr)[1], nested)
				if err != nil {
					regErr = err
					return attr
				}
				return `URI="` + local + `"`
			})
			if regErr != nil {
				return "", regErr
			}
			if strings.HasPrefix(trimmed, "#EXT-X-STREAM-INF") {
				nextIsVariant = true
			}
			out = append(out, line)
		default:
			local, err := register(trimmed, nextIsVariant)
			if err != nil {
				return "", err
			}
			nextIsVariant = false
			out = append(out, local)
		}
	}
	return strings.Join(out, "\n"), nil
}

func isPlaylistURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && strings.HasSuffix(strings.ToLower(u.Path), ".m3u8")
}

// serveSegment streams a segment with its PNG wrapper, if any, removed.
func serveSegment(w http.ResponseWriter, r *http.Request, resp *http.Response) {
	br := bufio.NewReaderSize(resp.Body, 64<<10)
	skipped, err := skipPNGWrapper(br)
	if err != nil {
		util.Debug("HiAnime HLS proxy could not unwrap a segment", "err", err)
		http.Error(w, "segment unwrap failed", http.StatusBadGateway)
		return
	}
	skipped += skipToTSPacket(br)
	w.Header().Set("Content-Type", "video/mp2t")
	if resp.ContentLength >= skipped {
		w.Header().Set("Content-Length", strconv.FormatInt(resp.ContentLength-skipped, 10))
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := io.Copy(w, br); err != nil && r.Context().Err() == nil {
		util.Debug("HiAnime HLS proxy copy ended", "err", err)
	}
}

// skipPNGWrapper consumes a whole PNG image from the front of br, chunk by
// chunk up to and including IEND, and reports how many bytes it skipped. A
// stream that does not start with the PNG signature is left untouched.
func skipPNGWrapper(br *bufio.Reader) (int64, error) {
	sig, err := br.Peek(len(pngSignature))
	if err != nil || !bytes.Equal(sig, pngSignature) {
		return 0, nil //nolint:nilerr // a short or plain segment is passed through as is
	}
	if _, err := br.Discard(len(pngSignature)); err != nil {
		return 0, err
	}
	skipped := int64(len(pngSignature))
	var hdr [8]byte // length, type
	for {
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			return 0, fmt.Errorf("PNG wrapper ended early: %w", err)
		}
		n := binary.BigEndian.Uint32(hdr[:4])
		if n > maxPNGChunk {
			return 0, fmt.Errorf("PNG wrapper chunk of %d bytes", n)
		}
		if _, err := br.Discard(int(n) + 4); err != nil { // data + CRC
			return 0, fmt.Errorf("PNG wrapper ended early: %w", err)
		}
		skipped += 8 + int64(n) + 4
		if string(hdr[4:]) == "IEND" {
			return skipped, nil
		}
	}
}

// tsPacketSize is the MPEG-TS packet length; every packet starts with tsSync.
const (
	tsPacketSize = 188
	tsSync       = 0x47
)

// skipToTSPacket drops the bytes before the first whole TS packet and reports
// how many it dropped. Live MegaPlay segments open with a 182-byte packet (an
// SDT short of its stuffing), so the 188-byte cadence only starts after it.
// Read from a file ffmpeg recovers by seeking back, but inside HLS it cannot:
// it logged "mpegts: Unable to seek back to the start", lost the first ~17s of
// the episode and made that the start time. mpv rebases the video on it while
// external subtitles keep their own clock, so every line came ~17s late (seen
// 2026-10-19 on Black Clover EP 85). A segment already aligned, or too short
// to tell, is left untouched.
func skipToTSPacket(br *bufio.Reader) int64 {
	head, _ := br.Peek(3 * tsPacketSize)
	aligned := func(o int) bool {
		return o+2*tsPacketSize < len(head) &&
			head[o] == tsSync && head[o+tsPacketSize] == tsSync && head[o+2*tsPacketSize] == tsSync
	}
	if len(head) < 3*tsPacketSize || aligned(0) {
		return 0
	}
	for o := 1; o < tsPacketSize; o++ {
		if aligned(o) {
			_, _ = br.Discard(o)
			return int64(o)
		}
	}
	return 0
}
