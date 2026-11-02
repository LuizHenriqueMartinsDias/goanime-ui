package player

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/alvarorichard/Goanime/internal/upscaler"
	"github.com/alvarorichard/Goanime/internal/util"
)

// WebPlayback is a stream prepared for mpv by the web UI: the final media
// target plus the per-playback argument list the CLI would build for it.
type WebPlayback struct {
	URL   string
	Args  []string
	IsHLS bool
	// LaterSubs are the subtitle URLs left out of Args, for AddSubtitles once
	// the video is playing.
	LaterSubs []string
}

// PrepareWebPlayback mirrors the non-interactive half of playVideo: Blogger
// resolution, URL normalization and buildPlaybackArgs. It must run right after
// the stream was resolved, while the scraper's global referer/user agent still
// belong to this episode. Call StopBloggerProxy once mpv exits.
//
// subs are the tracks the source resolved for this episode; subLang is the
// viewer's preference ("pt", "en" or "off", see webSubtitleArgs).
func PrepareWebPlayback(videoURL, title string, resumeSecs int, subs []util.SubtitleInfo, subLang string) WebPlayback {
	if strings.Contains(videoURL, "blogger.com/video.g") {
		if resolved, err := extractBloggerGoogleVideoURL(videoURL); err == nil && resolved != "" {
			videoURL = resolved
		}
	}
	videoURL = strings.Replace(videoURL, "720pp.mp4", "720p.mp4", 1)

	shaderArgs := upscaler.GetMPVShaderArgs(upscaler.GetShaderMode())
	isHLS := LooksLikeHLS(videoURL)
	subArgs, laterSubs := webSubtitleArgs(subs, subLang)
	args := buildPlaybackArgs(playbackArgsInput{
		VideoURL:         videoURL,
		IsHLS:            isHLS,
		Is9Anime:         util.Is9AnimeSource(),
		UpscalingEnabled: len(shaderArgs) > 0,
		ShaderArgs:       shaderArgs,
		Wayland:          runtime.GOOS == "linux" && os.Getenv("WAYLAND_DISPLAY") != "",
		Title:            title,
		ResumeTime:       resumeSecs,
		SubArgs:          subArgs,
	})
	return WebPlayback{URL: videoURL, Args: args, IsHLS: isHLS, LaterSubs: laterSubs}
}

// subtitleLangWords maps a subtitle preference to the word every track label
// in that language carries ("Portuguese (- Brazilian)", "English").
var subtitleLangWords = map[string]string{"pt": "portug", "en": "english"}

// webSubtitleArgs splits the tracks the source resolved into the mpv arguments
// to start with and the URLs to add once the video plays. mpv fetches each
// --sub-file one after the other before it opens the window, so only the track
// that will be shown goes on the command line: the preferred language, or the
// first track when none matches. "off" starts with none selected. Any other
// preference falls back to Portuguese.
func webSubtitleArgs(subs []util.SubtitleInfo, subLang string) (args, later []string) {
	word, ok := subtitleLangWords[subLang]
	if !ok {
		word = subtitleLangWords["pt"]
	}
	var preferred, rest []string
	for _, s := range subs {
		if s.URL == "" {
			continue
		}
		if subLang != "off" && strings.Contains(strings.ToLower(s.Label+" "+s.Language), word) {
			preferred = append(preferred, s.URL)
		} else {
			rest = append(rest, s.URL)
		}
	}
	urls := append(preferred, rest...)
	if len(urls) == 0 {
		return nil, nil
	}
	if subLang == "off" {
		return []string{"--sid=no"}, urls
	}
	return []string{"--sub-file=" + urls[0]}, urls[1:]
}

// ErrMpvGone means mpv's IPC socket stopped answering: the player was closed.
var ErrMpvGone = errors.New("mpv is not running")

// ErrPropertyUnavailable means mpv is running but cannot report the property
// yet, as with time-pos while a stream is still loading.
var ErrPropertyUnavailable = errors.New("mpv property unavailable")

// MpvGet reads one property over mpv's IPC socket. Unlike MpvSendCommand it
// reads the whole reply however long it is (track-list passes 4 KiB with a
// handful of external subtitles), skips the events mpv interleaves, and gives
// up after a few seconds instead of blocking.
func MpvGet(socketPath, name string) (any, error) {
	return mpvRequest(socketPath, []any{"get_property", name}, 3*time.Second)
}

func mpvRequest(socketPath string, command []any, timeout time.Duration) (any, error) {
	conn, err := dialMPVSocket(socketPath)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMpvGone, err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(timeout))

	req, err := json.Marshal(map[string]any{"command": command})
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(append(req, '\n')); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMpvGone, err)
	}
	r := bufio.NewReader(conn)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrMpvGone, err)
		}
		var resp struct {
			Data  any    `json:"data"`
			Error string `json:"error"`
			Event string `json:"event"`
		}
		if json.Unmarshal(line, &resp) != nil || resp.Event != "" {
			continue
		}
		switch resp.Error {
		case "success":
			return resp.Data, nil
		case "property unavailable":
			return nil, ErrPropertyUnavailable
		default:
			return nil, fmt.Errorf("mpv: %s", resp.Error)
		}
	}
}

// AddSubtitlesOncePlaying waits for the video to start, since sub-add fails
// until then, and loads urls one at a time. They stay unselected, and mpv runs
// sub-add on its own thread, so playback does not pause for them. A track that
// fails is skipped; it returns when mpv closes.
func AddSubtitlesOncePlaying(socketPath string, urls []string) {
	const pollInterval = 250 * time.Millisecond
	deadline := time.Now().Add(2 * time.Minute)
	for {
		_, err := MpvGet(socketPath, "time-pos")
		if err == nil {
			break
		}
		if errors.Is(err, ErrMpvGone) || time.Now().After(deadline) {
			return
		}
		time.Sleep(pollInterval)
	}
	for _, u := range urls {
		_, err := mpvRequest(socketPath, []any{"sub-add", u, "auto"}, 30*time.Second)
		if errors.Is(err, ErrMpvGone) {
			return
		}
		if err != nil {
			util.Debugf("subtitle %s not loaded: %v", u, err)
		}
	}
}

// WaitForVideoReady reports whether mpv started playing the stream behind
// socketPath. HLS resume needs it: --start is unreliable there, so the caller
// seeks once the video is ready.
func WaitForVideoReady(socketPath string) bool {
	return waitForVideoReady(socketPath)
}
