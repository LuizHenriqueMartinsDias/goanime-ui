package player

import (
	"bufio"
	"errors"
	"net"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/alvarorichard/Goanime/internal/util"
)

func TestWebSubtitleArgsStartsWithOnlyThePreferredTrack(t *testing.T) {
	subs := []util.SubtitleInfo{
		{URL: "https://cdn.test/en.vtt", Label: "English"},
		{URL: "", Label: "Broken"},
		{URL: "https://cdn.test/pt.vtt", Label: "Portuguese (- Brazilian)"},
		{URL: "https://cdn.test/es.vtt", Label: "Spanish"},
	}
	const en, pt, es = "https://cdn.test/en.vtt", "https://cdn.test/pt.vtt", "https://cdn.test/es.vtt"

	for _, tc := range []struct {
		name, pref string
		subs       []util.SubtitleInfo
		args       []string
		later      []string
	}{
		{"portuguese", "pt", subs, []string{"--sub-file=" + pt}, []string{en, es}},
		{"unknown preference", "", subs, []string{"--sub-file=" + pt}, []string{en, es}},
		{"english", "en", subs, []string{"--sub-file=" + en}, []string{pt, es}},
		{"no track in that language", "pt", subs[:1], []string{"--sub-file=" + en}, []string{}},
		{"off", "off", subs, []string{"--sid=no"}, []string{en, pt, es}},
		{"off without tracks", "off", nil, nil, nil},
		{"no tracks", "pt", nil, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args, later := webSubtitleArgs(tc.subs, tc.pref)
			assert.Equal(t, tc.args, args)
			assert.Equal(t, tc.later, later)
		})
	}
}

// scriptedMpv answers each request with the next reply, recording the commands.
// Once the replies run out it closes the socket, as an mpv that exited would.
func scriptedMpv(t *testing.T, replies ...string) (string, func() []string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("mpv IPC is a named pipe on Windows")
	}
	sock := filepath.Join(t.TempDir(), "mpv.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var (
		mu   sync.Mutex
		seen []string
	)
	go func() {
		for _, reply := range replies {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(conn).ReadString('\n')
			mu.Lock()
			seen = append(seen, strings.TrimSpace(line))
			mu.Unlock()
			_, _ = conn.Write([]byte(reply + "\n"))
			_ = conn.Close()
		}
		_ = ln.Close()
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return sock, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

const (
	mpvUnavailable = `{"request_id":0,"error":"property unavailable"}`
	mpvPlaying     = `{"data":1.5,"request_id":0,"error":"success"}`
	mpvAdded       = `{"request_id":0,"error":"success"}`
	mpvFailed      = `{"request_id":0,"error":"error running command"}`
)

func TestAddSubtitlesWaitsForPlaybackAndSkipsFailures(t *testing.T) {
	sock, seen := scriptedMpv(t, mpvUnavailable, mpvPlaying, mpvFailed, mpvAdded)
	AddSubtitlesOncePlaying(sock, []string{"https://cdn.test/a.vtt", "https://cdn.test/b.vtt"})

	got := seen()
	assert.Len(t, got, 4)
	assert.Contains(t, got[0], "time-pos")
	assert.Contains(t, got[1], "time-pos")
	assert.JSONEq(t, `{"command":["sub-add","https://cdn.test/a.vtt","auto"]}`, got[2])
	assert.JSONEq(t, `{"command":["sub-add","https://cdn.test/b.vtt","auto"]}`, got[3])
}

func TestAddSubtitlesStopsWhenMpvCloses(t *testing.T) {
	sock, seen := scriptedMpv(t, mpvPlaying)
	AddSubtitlesOncePlaying(sock, []string{"https://cdn.test/a.vtt", "https://cdn.test/b.vtt"})
	assert.Len(t, seen(), 1, "nothing is sent once the socket is gone")
}

// fakeMpv answers each request on a unix socket with an event line followed by
// reply.
func fakeMpv(t *testing.T, reply string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("mpv IPC is a named pipe on Windows")
	}
	sock := filepath.Join(t.TempDir(), "mpv.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = bufio.NewReader(conn).ReadBytes('\n')
			_, _ = conn.Write([]byte(`{"event":"playback-restart"}` + "\n" + reply + "\n"))
			_ = conn.Close()
		}
	}()
	return sock
}

func TestMpvGetReadsLongRepliesPastEvents(t *testing.T) {
	long := strings.Repeat("x", 10000)
	v, err := MpvGet(fakeMpv(t, `{"data":"`+long+`","request_id":0,"error":"success"}`), "track-list")
	assert.NoError(t, err)
	assert.Equal(t, long, v)
}

func TestMpvGetErrors(t *testing.T) {
	_, err := MpvGet(fakeMpv(t, `{"request_id":0,"error":"property unavailable"}`), "time-pos")
	assert.True(t, errors.Is(err, ErrPropertyUnavailable), err)

	_, err = MpvGet(filepath.Join(t.TempDir(), "missing.sock"), "time-pos")
	assert.True(t, errors.Is(err, ErrMpvGone), err)
}
