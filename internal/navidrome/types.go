package navidrome

import (
	"cmp"
	"encoding/json"
	"strings"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/media"
)

// envelope wraps every Subsonic answer under the "subsonic-response" key.
type envelope struct {
	Inner response `json:"subsonic-response"`
}

type response struct {
	Status        string        `json:"status"`
	Version       string        `json:"version"`
	Type          string        `json:"type"`
	ServerVersion string        `json:"serverVersion"`
	Error         *apiError     `json:"error"`
	MusicFolders  musicFolders  `json:"musicFolders"`
	SearchResult3 searchResult3 `json:"searchResult3"`
	Song          *Child        `json:"song"`
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type musicFolders struct {
	MusicFolder []MusicFolder `json:"musicFolder"`
}

// MusicFolder is one top-level library. Subsonic numbers them, but Navidrome
// and OpenSubsonic may send a string id, so json.Number accepts both.
type MusicFolder struct {
	ID   json.Number `json:"id"`
	Name string      `json:"name"`
}

type searchResult3 struct {
	Song []Child `json:"song"`
}

// Child is a song in a Subsonic answer. samplingRate, bitDepth, displayArtist
// and displayAlbumArtist are OpenSubsonic fields that Navidrome fills in;
// classic Subsonic servers send artist and albumArtist instead, so convert
// reads whichever is present.
type Child struct {
	ID                 string `json:"id"`
	Title              string `json:"title"`
	Artist             string `json:"artist"`
	AlbumArtist        string `json:"albumArtist"`
	DisplayArtist      string `json:"displayArtist"`
	DisplayAlbumArtist string `json:"displayAlbumArtist"`
	Album              string `json:"album"`
	Duration           int    `json:"duration"`
	Suffix             string `json:"suffix"`
	ContentType        string `json:"contentType"`
	BitRate            int    `json:"bitRate"`
	SamplingRate       int    `json:"samplingRate"`
	BitDepth           int    `json:"bitDepth"`
	CoverArt           string `json:"coverArt"`
	IsDir              bool   `json:"isDir"`
}

func (c Child) convert(client *Client) media.Track {
	container := strings.ToLower(c.Suffix)
	out := media.Track{
		ID:          c.ID,
		Title:       c.Title,
		Artist:      cmp.Or(c.DisplayArtist, c.Artist, c.AlbumArtist, c.DisplayAlbumArtist),
		AlbumArtist: cmp.Or(c.AlbumArtist, c.DisplayAlbumArtist),
		Album:       c.Album,
		DurationSec: c.Duration,
		Codec:       media.AudioFormat(container, container),
		Container:   container,
		BitrateKbps: c.BitRate,
		SampleRate:  c.SamplingRate,
		BitDepth:    c.BitDepth,
	}
	if c.ID != "" {
		out.FileRef = client.streamPath(c.ID)
	}
	if c.CoverArt != "" {
		out.ArtRef = client.artPath(c.CoverArt)
	}
	return out
}
