package hianime

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/alvarorichard/Goanime/internal/scraper/netx"
	"github.com/alvarorichard/Goanime/internal/util"
	"github.com/alvarorichard/Goanime/internal/util/jsonx"
)

// The MegaPlay servers ("HD-1", "HD-2", …) are the fallback for ZokoAnime.
//
// The chain, measured 2026-10-15 on megaplay.buzz:
//
//	GET <embed>                                 → page with
//	                                              <div id="megaplay-player" data-id="…">
//	GET <origin>/stream/getSources?id=<data-id> → {"tracks":[…],"enc":"…"}
//
// "enc" is base64url of AES-256-CBC over {"file":"<master.m3u8>"}. Key and IV
// are short ASCII strings zero-padded to 32 and 16 bytes, and the player's own
// lib/app.main.js carries them as DOMAINS_ENC_KEY / DOMAINS_ENC_IV, so they are
// read from there on every resolve and the constants below are only the
// fallback for when that script cannot be read.
//
// The playlist's segments are MPEG-TS hidden behind a 1×1 PNG and served from
// an image CDN; hlsproxy.go unwraps them for mpv.
const (
	megaPlayFallbackKey = "i?LMTAx0Q6,:}50U"
	megaPlayFallbackIV  = "W0;27ToaUpl_P%'c"
)

var (
	// megaPlayPathRe matches a MegaPlay embed path. ZokoAnime's is
	// /stream/mal/…, so the s-<n> segment is what tells them apart.
	megaPlayPathRe = regexp.MustCompile(`^/stream/s-\d+/`)
	// megaPlayPlayerRe captures the data-id of the player element.
	megaPlayPlayerRe = regexp.MustCompile(`id="megaplay-player"[^>]*?\sdata-id="(\d+)"`)
	// megaPlayScriptRe captures the src of the script that carries the key.
	megaPlayScriptRe = regexp.MustCompile(`src="([^"]*/app\.main\.js[^"]*)"`)
	megaPlayKeyRe    = regexp.MustCompile(`DOMAINS_ENC_KEY\s*=\s*"([^"]+)"`)
	megaPlayIVRe     = regexp.MustCompile(`DOMAINS_ENC_IV\s*=\s*"([^"]+)"`)
)

// megaPlayServers lists the MegaPlay entries, preferred audio first. They are
// recognised by their embed path (/stream/s-2/<id>/<audio>) rather than their
// name, which the site renumbers freely, or their host, which rotates.
func megaPlayServers(servers []server, prefer string) []server {
	var preferred, rest []server
	for _, s := range servers {
		if !isMegaPlayEmbed(s.Embed) {
			continue
		}
		if s.Audio == prefer {
			preferred = append(preferred, s)
		} else {
			rest = append(rest, s)
		}
	}
	return append(preferred, rest...)
}

func isMegaPlayEmbed(embed string) bool {
	u, err := url.Parse(embed)
	return err == nil && u.Host != "" && megaPlayPathRe.MatchString(u.Path)
}

// megaPlaySources is the getSources answer.
type megaPlaySources struct {
	Enc    string `json:"enc"`
	Tracks []struct {
		File  string `json:"file"`
		Label string `json:"label"`
		Kind  string `json:"kind"`
	} `json:"tracks"`
}

// resolveMegaPlay reads a MegaPlay embed and returns its stream through the
// local HLS proxy.
func (c *HiAnimeClient) resolveMegaPlay(ctx context.Context, chosen server, quality string) (string, map[string]string, error) {
	origin := originOf(chosen.Embed)
	page, err := c.getBody(ctx, chosen.Embed, "embed", func(req *http.Request) {
		req.Header.Set("Referer", c.baseURL+"/")
	})
	if err != nil {
		return "", nil, err
	}
	m := megaPlayPlayerRe.FindSubmatch(page)
	if m == nil {
		return "", nil, netx.NewParserError(sourceLabel, "embed",
			"no MegaPlay player id in the embed page (layout changed?)", nil)
	}

	sourcesURL := origin + "/stream/getSources?id=" + string(m[1])
	if u, err := url.Parse(chosen.Embed); err == nil && u.Query().Get("s") != "" {
		sourcesURL += "&s=" + url.QueryEscape(u.Query().Get("s"))
	}
	body, err := c.getBody(ctx, sourcesURL, "sources", func(req *http.Request) {
		req.Header.Set("Referer", chosen.Embed)
		req.Header.Set("Accept", "application/json, text/javascript, */*; q=0.01")
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
	})
	if err != nil {
		return "", nil, err
	}
	var sources megaPlaySources
	if err := jsonx.Unmarshal(body, &sources); err != nil {
		return "", nil, netx.NewParserError(sourceLabel, "sources", "malformed JSON response", err)
	}
	if sources.Enc == "" {
		return "", nil, netx.NewParserError(sourceLabel, "sources", "the answer carried no stream", nil)
	}

	key, iv := c.megaPlayKey(ctx, origin, page)
	master, err := decryptMegaPlayFile(sources.Enc, key, iv)
	if err != nil && (key != megaPlayFallbackKey || iv != megaPlayFallbackIV) {
		master, err = decryptMegaPlayFile(sources.Enc, megaPlayFallbackKey, megaPlayFallbackIV)
	}
	if err != nil {
		return "", nil, err
	}

	referer := origin + "/"
	streamURL := master
	if v, ok := c.selectVariant(ctx, master, quality, referer); ok {
		streamURL = v
	}
	if err := c.probePlaylist(ctx, streamURL, referer); err != nil {
		return "", nil, err
	}
	local, err := sharedHLSProxy().serve(streamURL, referer, c.userAgent, c.stream)
	if err != nil {
		return "", nil, err
	}

	metadata := map[string]string{
		"source":     "hianime",
		"referer":    referer,
		"audio_lang": chosen.Audio,
	}
	var subs []embedSubtitle
	for _, t := range sources.Tracks {
		if t.Kind == "captions" || t.Kind == "subtitles" {
			subs = append(subs, embedSubtitle{Label: t.Label, Src: t.File})
		}
	}
	if encoded := encodeSubtitles(subs); encoded != "" {
		metadata["subtitles"] = encoded
	}
	return local, metadata, nil
}

// megaPlayKey reads the key and IV out of the player's app.main.js, falling
// back to the constants when the script cannot be read or no longer has them.
func (c *HiAnimeClient) megaPlayKey(ctx context.Context, origin string, page []byte) (key, iv string) {
	scriptURL := origin + "/lib/app.main.js"
	if m := megaPlayScriptRe.FindSubmatch(page); m != nil {
		scriptURL = resolveRef(origin+"/", string(m[1]))
	}
	script, err := c.getBody(ctx, scriptURL, "player script", func(req *http.Request) {
		req.Header.Set("Referer", origin+"/")
	})
	if err != nil {
		util.Debug("HiAnime could not read the MegaPlay script; using the known key", "error", err)
		return megaPlayFallbackKey, megaPlayFallbackIV
	}
	k, v := megaPlayKeyRe.FindSubmatch(script), megaPlayIVRe.FindSubmatch(script)
	if k == nil || v == nil {
		util.Debug("HiAnime MegaPlay script carries no key; using the known key")
		return megaPlayFallbackKey, megaPlayFallbackIV
	}
	return string(k[1]), string(v[1])
}

// decryptMegaPlayFile decrypts a getSources "enc" field into the stream URL.
func decryptMegaPlayFile(enc, key, iv string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(enc, "="))
	if err != nil {
		return "", netx.NewParserError(sourceLabel, "sources", "the stream field was not base64url", err)
	}
	if len(raw) == 0 || len(raw)%aes.BlockSize != 0 {
		return "", netx.NewParserError(sourceLabel, "sources", "the stream field is not whole AES blocks", nil)
	}
	block, err := aes.NewCipher(zeroPadded(key, 32))
	if err != nil {
		return "", netx.NewParserError(sourceLabel, "sources", "bad key", err)
	}
	plain := make([]byte, len(raw))
	cipher.NewCBCDecrypter(block, zeroPadded(iv, aes.BlockSize)).CryptBlocks(plain, raw)

	pad := int(plain[len(plain)-1])
	if pad == 0 || pad > aes.BlockSize || pad > len(plain) {
		return "", netx.NewParserError(sourceLabel, "sources", "the stream field did not decrypt (key changed?)", nil)
	}
	var out struct {
		File string `json:"file"`
	}
	if err := jsonx.Unmarshal(plain[:len(plain)-pad], &out); err != nil || strings.TrimSpace(out.File) == "" {
		return "", netx.NewParserError(sourceLabel, "sources", "the stream field did not decrypt (key changed?)", err)
	}
	return out.File, nil
}

// zeroPadded is the player's key derivation: the string's bytes, cut or
// zero-filled to n.
func zeroPadded(s string, n int) []byte {
	b := make([]byte, n)
	copy(b, s)
	return b
}
