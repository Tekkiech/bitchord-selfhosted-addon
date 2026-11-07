// Package navidrome talks to a Navidrome server through its Subsonic API.
// Authentication is the Subsonic salted-token scheme: the password never
// leaves the addon as plaintext, only md5(password + salt) does, and every
// request carries the username, token and salt as query parameters.
package navidrome

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/media"
)

// clientName names the addon to Navidrome; subsonicVersion is the protocol
// version the addon speaks, not the addon's own version.
const (
	clientName      = "bitchord-selfhosted-addon"
	subsonicVersion = "1.16.1"
	// allSongs is the Subsonic idiom for "match everything": search3 with a
	// query of two double-quotes returns the whole library.
	allSongs = `""`
)

type Options struct {
	BaseURL       string
	Username      string
	Password      string
	PageSize      int
	HeaderTimeout time.Duration
	CallTimeout   time.Duration
}

type Client struct {
	http     *media.Client
	username string
	token    string
	salt     string
	pageSize int
}

func New(o Options) *Client {
	if o.PageSize <= 0 {
		o.PageSize = 500
	}
	salt, token := credentials(o.Password)
	return &Client{
		http: media.NewClient(media.ClientOptions{
			BaseURL:       o.BaseURL,
			Name:          "navidrome",
			TokenVar:      "the Navidrome password",
			HeaderTimeout: o.HeaderTimeout,
			CallTimeout:   o.CallTimeout,
		}),
		username: o.Username,
		token:    token,
		salt:     salt,
		pageSize: o.PageSize,
	}
}

// credentials turns a password into a fresh salt and the Subsonic token
// md5(password + salt). The salt is random per client and reused for its
// lifetime, which the Subsonic spec allows.
func credentials(password string) (salt, token string) {
	raw := make([]byte, 8)
	rand.Read(raw)
	salt = hex.EncodeToString(raw)
	sum := md5.Sum([]byte(password + salt))
	return salt, hex.EncodeToString(sum[:])
}

func (c *Client) Name() string { return "Navidrome" }

// authParams are the credentials every request carries. f=json asks for JSON;
// the binary endpoints ignore it and answer with the file.
func (c *Client) authParams() url.Values {
	return url.Values{
		"u": {c.username},
		"t": {c.token},
		"s": {c.salt},
		"v": {subsonicVersion},
		"c": {clientName},
		"f": {"json"},
	}
}

func (c *Client) path(endpoint string, params url.Values) string {
	q := c.authParams()
	for key, values := range params {
		for _, value := range values {
			q.Add(key, value)
		}
	}
	return "/rest/" + endpoint + "?" + q.Encode()
}

// get calls a JSON endpoint and turns a Subsonic error into the shared media
// errors. Navidrome answers 200 even for a bad password, with the failure in
// the body, so the status is read from there rather than from HTTP.
func (c *Client) get(ctx context.Context, endpoint string, params url.Values) (response, error) {
	var env envelope
	if err := c.http.GetJSON(ctx, c.path(endpoint, params), nil, &env); err != nil {
		return response{}, err
	}
	r := env.Inner
	if r.Status == "ok" {
		return r, nil
	}
	if r.Error == nil {
		return r, errors.New("navidrome: the request failed")
	}
	switch r.Error.Code {
	case 40, 41, 44:
		return r, fmt.Errorf("navidrome rejected %s: %w", "the username or password", media.ErrUnauthorized)
	case 50:
		return r, fmt.Errorf("navidrome: the user is not allowed to do this: %w", media.ErrUnauthorized)
	case 70:
		return r, media.ErrNotFound
	default:
		return r, fmt.Errorf("navidrome: %s (code %d)", r.Error.Message, r.Error.Code)
	}
}

func (c *Client) Version(ctx context.Context) (string, error) {
	r, err := c.get(ctx, "ping.view", nil)
	if err != nil {
		return "", err
	}
	if r.ServerVersion != "" {
		return r.ServerVersion, nil
	}
	return r.Version, nil
}

func (c *Client) musicFolders(ctx context.Context) ([]MusicFolder, error) {
	r, err := c.get(ctx, "getMusicFolders.view", nil)
	if err != nil {
		return nil, err
	}
	return r.MusicFolders.MusicFolder, nil
}

func (c *Client) Libraries(ctx context.Context) ([]media.Library, error) {
	folders, err := c.musicFolders(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]media.Library, len(folders))
	for i, folder := range folders {
		out[i] = media.Library{ID: folder.ID.String(), Name: folder.Name}
	}
	return out, nil
}

// resolveFolder maps a library filter (an id or a name) to a music-folder id
// for search3, or "" for the whole server.
func (c *Client) resolveFolder(ctx context.Context, filter string) (string, error) {
	if filter == "" {
		return "", nil
	}
	folders, err := c.musicFolders(ctx)
	if err != nil {
		return "", err
	}
	for _, folder := range folders {
		if folder.ID.String() == filter || folder.Name == filter {
			return folder.ID.String(), nil
		}
	}
	return "", fmt.Errorf("navidrome: no music library matches %q", filter)
}

func (c *Client) AllTracks(ctx context.Context, filter string) ([]media.Track, error) {
	folderID, err := c.resolveFolder(ctx, filter)
	if err != nil {
		return nil, err
	}
	songs, err := media.AllPages(c.pageSize,
		func(s Child) string { return s.ID },
		func(start int) ([]Child, error) { return c.songPage(ctx, folderID, start) })
	if err != nil {
		return nil, err
	}
	out := make([]media.Track, len(songs))
	for i, song := range songs {
		out[i] = song.convert(c)
	}
	return out, nil
}

// songPage asks search3 for one page of every song. An empty query is the
// Subsonic way to match all; artists and albums are turned off so only songs
// come back.
func (c *Client) songPage(ctx context.Context, folderID string, start int) ([]Child, error) {
	params := url.Values{
		"query":       {allSongs},
		"artistCount": {"0"},
		"albumCount":  {"0"},
		"songCount":   {strconv.Itoa(c.pageSize)},
		"songOffset":  {strconv.Itoa(start)},
	}
	if folderID != "" {
		params.Set("musicFolderId", folderID)
	}
	r, err := c.get(ctx, "search3.view", params)
	if err != nil {
		return nil, err
	}
	return r.SearchResult3.Song, nil
}

func (c *Client) Track(ctx context.Context, id string) (media.Track, error) {
	r, err := c.get(ctx, "getSong.view", url.Values{"id": {id}})
	if err != nil {
		return media.Track{}, err
	}
	if r.Song == nil {
		return media.Track{}, media.ErrNotFound
	}
	return r.Song.convert(c), nil
}

// streamPath asks for the original file: format=raw turns off transcoding, and
// Navidrome serves it with a true Content-Length and byte-range support.
func (c *Client) streamPath(id string) string {
	q := c.authParams()
	q.Set("id", id)
	q.Set("format", "raw")
	return "/rest/stream.view?" + q.Encode()
}

func (c *Client) artPath(coverArt string) string {
	q := c.authParams()
	q.Set("id", coverArt)
	q.Set("size", "600")
	return "/rest/getCoverArt.view?" + q.Encode()
}

func (c *Client) OpenFile(ctx context.Context, track media.Track, method string, header http.Header) (*http.Response, error) {
	return c.http.Open(ctx, method, track.FileRef, header)
}

func (c *Client) OpenArt(ctx context.Context, track media.Track) (*http.Response, error) {
	return c.http.Open(ctx, http.MethodGet, track.ArtRef, nil)
}
