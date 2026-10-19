package hianime

import (
	"bufio"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A getSources "enc" captured from megaplay.buzz on 2026-10-15 (Black Clover
// EP 23, sub), decrypted with the key app.main.js carried that day.
func TestDecryptMegaPlayFile_LiveVector(t *testing.T) {
	t.Parallel()
	const enc = "wdeBruh3qqn_i5wUNnyaPaopIIVfK48gK5UCwNjNCsjoYJypy5UmDk-Uvxs3p3ZxMJqyrOvW6UWWAsxKn8l4tqwc9Xn8XPnXmwDbemYGBE2IxGl0RxB0RsuBk75WUqPTegJZcPVCl0V3j69yOe5JamxICzpa-CTPBfA762DvF0A"
	got, err := decryptMegaPlayFile(enc, megaPlayFallbackKey, megaPlayFallbackIV)
	require.NoError(t, err)
	assert.Equal(t, "https://megap.shiora.top/8208974663db80265e9bfe7b222dcb18/fce161685354e4fb6cf57d5c5f3bbf49/master.m3u8", got)
}

func TestDecryptMegaPlayFile_WrongKeySaysSo(t *testing.T) {
	t.Parallel()
	enc := encryptMegaPlay(t, `{"file":"https://cdn.example/master.m3u8"}`, "some-other-key", "some-other-iv")
	_, err := decryptMegaPlayFile(enc, megaPlayFallbackKey, megaPlayFallbackIV)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "did not decrypt")
}

func TestIsMegaPlayEmbed(t *testing.T) {
	t.Parallel()
	assert.True(t, isMegaPlayEmbed("https://megaplay.buzz/stream/s-2/27399/sub?s=tcdn"))
	assert.True(t, isMegaPlayEmbed("https://rotated.example/stream/s-5/1/dub"))
	assert.False(t, isMegaPlayEmbed("https://zokoanime.video/stream/mal/34572/23/sub"))
	assert.False(t, isMegaPlayEmbed("megaplay"))
}

// pngWrapper is the 1×1 image seen in front of every live segment, byte for
// byte: signature, IHDR, IDAT, IEND — 70 bytes.
var pngWrapper = []byte("\x89PNG\r\n\x1a\n" +
	"\x00\x00\x00\x0dIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89" +
	"\x00\x00\x00\x0dIDATx\xdac\x64\xf8\xcf\x50\x0f\x00\x03\x86\x01\x80\x5a\x34\x7d\x6b" +
	"\x00\x00\x00\x00IEND\xaeB`\x82")

// tsPayload stands in for MPEG-TS: two packets starting with the sync byte.
var tsPayload = append(append([]byte{0x47}, bytes.Repeat([]byte{1}, 187)...),
	append([]byte{0x47}, bytes.Repeat([]byte{2}, 187)...)...)

func TestSkipPNGWrapper(t *testing.T) {
	t.Parallel()
	require.Len(t, pngWrapper, 70)

	br := bufio.NewReader(bytes.NewReader(append(append([]byte{}, pngWrapper...), tsPayload...)))
	n, err := skipPNGWrapper(br)
	require.NoError(t, err)
	assert.EqualValues(t, 70, n)
	rest, _ := io.ReadAll(br)
	assert.Equal(t, tsPayload, rest)

	// A plain segment is passed through untouched.
	br = bufio.NewReader(bytes.NewReader(tsPayload))
	n, err = skipPNGWrapper(br)
	require.NoError(t, err)
	assert.Zero(t, n)
	rest, _ = io.ReadAll(br)
	assert.Equal(t, tsPayload, rest)

	// A wrapper cut before IEND is an error, not a segment of PNG bytes.
	_, err = skipPNGWrapper(bufio.NewReader(bytes.NewReader(pngWrapper[:40])))
	require.Error(t, err)
}

func TestSkipToTSPacket(t *testing.T) {
	t.Parallel()
	packets := bytes.Repeat(tsPayload, 2)

	// Live segments open with a packet 6 bytes short; it is dropped.
	short := append(append([]byte{0x47}, bytes.Repeat([]byte{0xff}, 181)...), packets...)
	br := bufio.NewReader(bytes.NewReader(short))
	assert.EqualValues(t, 182, skipToTSPacket(br))
	rest, _ := io.ReadAll(br)
	assert.Equal(t, packets, rest)

	// Aligned and too-short segments pass through untouched.
	for _, in := range [][]byte{packets, tsPayload} {
		br = bufio.NewReader(bytes.NewReader(in))
		assert.Zero(t, skipToTSPacket(br))
		rest, _ = io.ReadAll(br)
		assert.Equal(t, in, rest)
	}
}

// TestGetEpisodeStreamURL_FallsBackToMegaPlay runs the case this file exists
// for: the ZokoAnime CDN does not answer (a blocked host), so the episode is
// served by MegaPlay through the local proxy, and the segment mpv reads is
// plain MPEG-TS.
func TestGetEpisodeStreamURL_FallsBackToMegaPlay(t *testing.T) {
	t.Parallel()
	const key, iv = "test-key-from-js", "test-iv-from-js!"
	var base string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
		cdn := strings.HasPrefix(r.URL.Path, "/cdn/")
		if cdn && r.Header.Get("Referer") != base+"/" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		switch {
		case r.URL.Path == "/api/theme/episode/servers":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"status":true,"html":"`+
				`<div class=\"item server-item\" data-type=\"sub\" data-server-name=\"ZokoAnime\" data-hash=\"%s\"></div>`+
				`<div class=\"item server-item\" data-type=\"sub\" data-server-name=\"HD-1\" data-hash=\"%s\"></div>"}`,
				b64(base+"/zoko/sub"), b64(base+"/stream/s-2/77/sub?s=tcdn"))

		case r.URL.Path == "/zoko/sub":
			// A host nothing listens on: the connection is refused at once.
			_, _ = w.Write([]byte(embedPage(`{"src":"http://127.0.0.1:1/blocked/master.m3u8","subtitles":[]}`)))

		case r.URL.Path == "/stream/s-2/77/sub":
			fmt.Fprint(w, `<div class="fix-area" id="megaplay-player"
                data-id="103772"
                data-realid="77"></div>
				<script src="`+base+`/lib/app.main.js?v=2"></script>`)

		case r.URL.Path == "/lib/app.main.js":
			fmt.Fprintf(w, `const DOMAINS_ENC_KEY="%s",DOMAINS_ENC_IV="%s";`, key, iv)

		case r.URL.Path == "/stream/getSources":
			assert.Equal(t, "103772", r.URL.Query().Get("id"))
			assert.Equal(t, "tcdn", r.URL.Query().Get("s"))
			fmt.Fprintf(w, `{"tracks":[{"file":"%s/en.vtt","label":"English","kind":"captions"},`+
				`{"file":"%s/thumbs.vtt","kind":"thumbnails"}],"enc":"%s"}`,
				base, base, encryptMegaPlay(t, `{"file":"`+base+`/cdn/master.m3u8"}`, key, iv))

		case r.URL.Path == "/cdn/master.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1733370,RESOLUTION=1920x1080\nindex.m3u8\n"+
				`#EXT-X-I-FRAME-STREAM-INF:BANDWIDTH=41261,URI="iframes.m3u8"`+"\n")

		case r.URL.Path == "/cdn/index.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:11\n#EXTINF:3.795,\n"+base+"/cdn/seg0.image?sig=1\n#EXT-X-ENDLIST\n")

		case r.URL.Path == "/cdn/seg0.image":
			_, _ = w.Write(append(append([]byte{}, pngWrapper...), tsPayload...))

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	base = srv.URL
	t.Cleanup(srv.Close)

	got, meta, err := NewClientForTest(srv.URL).GetEpisodeStreamURL(
		context.Background(), srv.URL+"/watch/cowboy-bebop-26?ep=9001", "best")
	require.NoError(t, err)
	assert.Regexp(t, `^http://127\.0\.0\.1:\d+/hianime/[0-9a-f]+\.m3u8$`, got)
	assert.Equal(t, srv.URL+"/", meta["referer"])
	assert.Contains(t, meta["subtitles"], `"label":"English"`)
	assert.NotContains(t, meta["subtitles"], "thumbs.vtt")

	master := fetch(t, got)
	assert.NotContains(t, master, "iframes", "I-frame playlists index the wrapped bytes; they are dropped")
	variant := lastLine(master)
	assert.Regexp(t, `/hianime/[0-9a-f]+\.m3u8$`, variant)

	index := fetch(t, variant)
	segment := ""
	for line := range strings.SplitSeq(index, "\n") {
		if strings.HasPrefix(line, "http://127.0.0.1") {
			segment = line
		}
	}
	require.NotEmpty(t, segment, index)
	assert.True(t, strings.HasSuffix(segment, ".ts"))
	assert.Equal(t, string(tsPayload), fetch(t, segment), "the PNG wrapper must be cut off")

	resp, err := http.Get(strings.Replace(segment, ".ts", "0.ts", 1))
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "only registered URLs are served")
}

func fetch(t *testing.T, rawURL string) string {
	t.Helper()
	resp, err := http.Get(rawURL) // #nosec G107 -- local test proxy
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode, rawURL)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(body)
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

// encryptMegaPlay is the inverse of decryptMegaPlayFile.
func encryptMegaPlay(t *testing.T, plain, key, iv string) string {
	t.Helper()
	block, err := aes.NewCipher(zeroPadded(key, 32))
	require.NoError(t, err)
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	data := append([]byte(plain), bytes.Repeat([]byte{byte(pad)}, pad)...)
	cipher.NewCBCEncrypter(block, zeroPadded(iv, aes.BlockSize)).CryptBlocks(data, data)
	return base64.RawURLEncoding.EncodeToString(data)
}
