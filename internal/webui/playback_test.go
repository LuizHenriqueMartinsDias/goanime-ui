package webui

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alvarorichard/Goanime/internal/util"
)

func TestParseSubTracksNamesExternalTracksBySourceLabel(t *testing.T) {
	// The shape mpv's get_property track-list returns over IPC.
	var trackList any
	require.NoError(t, json.Unmarshal([]byte(`[
		{"id": 1, "type": "video", "selected": true},
		{"id": 1, "type": "sub", "external": true, "external-filename": "https://cdn.test/pt.vtt", "selected": true},
		{"id": 2, "type": "sub", "external": true, "external-filename": "https://cdn.test/en.vtt", "title": "en.vtt"},
		{"id": 3, "type": "sub", "lang": "spa"},
		{"id": 4, "type": "sub"}
	]`), &trackList))
	known := []util.SubtitleInfo{
		{URL: "https://cdn.test/en.vtt", Label: "English"},
		{URL: "https://cdn.test/pt.vtt", Label: "Portuguese (- Brazilian)"},
	}

	tracks, sid := parseSubTracks(trackList, known)

	assert.Equal(t, []SubTrack{
		{ID: 1, Label: "Portuguese (- Brazilian)"},
		{ID: 2, Label: "English"},
		{ID: 3, Label: "spa"},
		{ID: 4, Label: "Faixa 4"},
	}, tracks)
	assert.Equal(t, 1, sid)
}

func TestParseSubTracksWithSubtitlesOff(t *testing.T) {
	var trackList any
	require.NoError(t, json.Unmarshal([]byte(`[{"id": 1, "type": "sub", "selected": false}]`), &trackList))
	tracks, sid := parseSubTracks(trackList, nil)
	assert.Len(t, tracks, 1)
	assert.Zero(t, sid)

	tracks, sid = parseSubTracks(nil, nil)
	assert.Empty(t, tracks)
	assert.Zero(t, sid)
}
