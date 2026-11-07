package fakes

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/navidrome"
)

const (
	NavidromeUser     = "lyle"
	NavidromePassword = "correct horse"
)

// Music folder ids. Navidrome numbers folders, and sends the id as a JSON
// number, so the fake answers with bare integers.
const (
	NavidromeMusicFolder      = "1"
	NavidromeAudiobooksFolder = "2"
	// NavidromeMixedID exercises the server's id pattern: Navidrome ids are
	// opaque base64url-ish strings, not only hex.
	NavidromeMixedID = "aB_cD-1234"
)

type Navidrome struct {
	recorder
	URL          string
	User         string
	Password     string
	Folders      []navidrome.MusicFolder
	Songs        map[string][]navidrome.Child
	Files        map[string][]byte
	Art          map[string][]byte
	Extra        map[string]http.HandlerFunc
	IgnorePaging bool
	// OnSearch, when set, runs at the start of every search3 before the normal
	// answer; a test uses it to block a refresh until it chooses to release it.
	OnSearch func(r *http.Request)
}

// Remove drops a song, and its file, from every folder.
func (f *Navidrome) Remove(id string) {
	for folder, songs := range f.Songs {
		f.Songs[folder] = slices.DeleteFunc(songs, func(c navidrome.Child) bool { return c.ID == id })
	}
	delete(f.Files, id)
}

func NewNavidrome(t testing.TB) *Navidrome {
	f := &Navidrome{
		User:     NavidromeUser,
		Password: NavidromePassword,
		Folders:  NavidromeFolders(),
		Songs:    NavidromeSongs(),
		Files:    NavidromeFiles(),
		Art:      NavidromeArt(),
		Extra:    map[string]http.HandlerFunc{},
	}
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(server.Close)
	f.URL = server.URL
	return f
}

// endpoint is the Subsonic method name, with any ".view" suffix removed.
func endpoint(path string) string {
	return strings.TrimSuffix(strings.TrimPrefix(path, "/rest/"), ".view")
}

func (f *Navidrome) serve(w http.ResponseWriter, r *http.Request) {
	status, rawBody := f.record(r)
	if f.override(w, status, rawBody) {
		return
	}
	q := r.URL.Query()
	if u := q.Get("u"); u == "" {
		f.fail(w, 10, "missing parameter: 'u'")
		return
	}
	if !f.authentic(q.Get("u"), q.Get("t"), q.Get("s")) {
		f.fail(w, 40, "Wrong username or password")
		return
	}
	name := endpoint(r.URL.Path)
	if extra, ok := f.Extra[name]; ok {
		extra(w, r)
		return
	}
	switch name {
	case "ping":
		f.ok(w, "")
	case "getMusicFolders":
		f.ok(w, `"musicFolders":{"musicFolder":`+jsonOf(f.Folders)+`}`)
	case "search3":
		f.search3(w, r)
	case "getSong":
		f.getSong(w, r)
	case "stream":
		f.stream(w, r)
	case "getCoverArt":
		f.coverArt(w, r)
	default:
		f.fail(w, 0, "unknown endpoint "+name)
	}
}

// authentic checks the Subsonic salted token: t must equal md5(password+salt).
func (f *Navidrome) authentic(user, token, salt string) bool {
	if user != f.User || token == "" || salt == "" {
		return false
	}
	sum := md5.Sum([]byte(f.Password + salt))
	return hex.EncodeToString(sum[:]) == token
}

func (f *Navidrome) search3(w http.ResponseWriter, r *http.Request) {
	if f.OnSearch != nil {
		f.OnSearch(r)
	}
	q := r.URL.Query()
	query := q.Get("query")
	if query != "" && query != `""` {
		// The addon only ever asks for everything; a real query is out of scope.
		f.ok(w, `"searchResult3":{}`)
		return
	}
	var all []navidrome.Child
	if folder := q.Get("musicFolderId"); folder != "" {
		all = f.Songs[folder]
	} else {
		for _, mf := range f.Folders {
			all = append(all, f.Songs[mf.ID.String()]...)
		}
	}
	page := all
	if !f.IgnorePaging {
		offset, _ := strconv.Atoi(q.Get("songOffset"))
		count, err := strconv.Atoi(q.Get("songCount"))
		if err != nil {
			count = len(all)
		}
		offset = min(offset, len(all))
		page = all[offset:min(offset+count, len(all))]
	}
	f.ok(w, `"searchResult3":{"song":`+jsonOf(page)+`}`)
}

func (f *Navidrome) getSong(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	for _, songs := range f.Songs {
		for _, song := range songs {
			if song.ID == id {
				f.ok(w, `"song":`+jsonOf(song))
				return
			}
		}
	}
	f.fail(w, 70, "Song not found")
}

func (f *Navidrome) stream(w http.ResponseWriter, r *http.Request) {
	body, ok := f.Files[r.URL.Query().Get("id")]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "audio/flac")
	w.Header().Set("ETag", `"fake-etag"`)
	w.Header().Set("X-Navidrome-Fake", "1")
	http.ServeContent(w, r, "", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), bytes.NewReader(body))
}

func (f *Navidrome) coverArt(w http.ResponseWriter, r *http.Request) {
	body, ok := f.Art[r.URL.Query().Get("id")]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("X-Navidrome-Fake", "1")
	w.Write(body)
}

func (f *Navidrome) ok(w http.ResponseWriter, extra string) {
	w.Header().Set("Content-Type", "application/json")
	body := "{\"subsonic-response\":{\"status\":\"ok\",\"version\":\"1.16.1\",\"type\":\"navidrome\",\"serverVersion\":\"0.64.2\",\"openSubsonic\":true"
	if extra != "" {
		body += "," + extra
	}
	w.Write([]byte(body + "}}"))
}

func (f *Navidrome) fail(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	body := "{\"subsonic-response\":{\"status\":\"failed\",\"version\":\"1.16.1\",\"type\":\"navidrome\",\"serverVersion\":\"0.64.2\",\"openSubsonic\":true," +
		`"error":{"code":` + strconv.Itoa(code) + `,"message":` + strconv.Quote(message) + `}}}`
	w.Write([]byte(body))
}

func NavidromeFolders() []navidrome.MusicFolder {
	return []navidrome.MusicFolder{
		{ID: "1", Name: "Music"},
		{ID: "2", Name: "Audiobooks"},
	}
}

// NavidromeSong builds a minimal playable song for tests that assemble their
// own library.
func NavidromeSong(id, title, artist, album string) navidrome.Child {
	return song(id, title, artist, album, 180, "flac", 900, 44100, 16, "art-"+id)
}

func song(id, title, artist, album string, duration int, suffix string, bitRate, samplingRate, bitDepth int, coverArt string) navidrome.Child {
	return navidrome.Child{
		ID: id, Title: title, Artist: artist, DisplayArtist: artist, AlbumArtist: artist,
		DisplayAlbumArtist: artist, Album: album, Duration: duration, Suffix: suffix,
		ContentType: "audio/" + suffix, BitRate: bitRate, SamplingRate: samplingRate,
		BitDepth: bitDepth, CoverArt: coverArt,
	}
}

func NavidromeSongs() map[string][]navidrome.Child {
	return map[string][]navidrome.Child{
		"1": {
			song("101", "New Religion", "All Time Low, Teddy Swims", "Tell Me I’m Alive", 184, "flac", 1875, 48000, 24, "art-101"),
			song("102", "Endless Slaughter", "Limp Bizkit", "Endless Slaughter", 337, "mp3", 320, 44100, 0, "art-102"),
			song("103", "Closer", "Anberlin", "As You Found Me", 231, "wav", 1411, 44100, 16, "art-103"),
			song("104", "Stays Four the Same", "The Ready Set", "Stays Four The Same", 201, "mp3", 251, 0, 0, ""),
			song("105", "Small Town Girl", "Never Shout Never", "Small Town Girl", 218, "m4a", 320, 44100, 0, "art-105"),
			song(NavidromeMixedID, "Dashed", "Anberlin", "Cities", 200, "flac", 900, 44100, 16, "art-dashed"),
		},
		"2": {
			song("501", "Chapter One", "Some Author", "Some Book", 1800, "mp3", 64, 44100, 0, ""),
		},
	}
}

// NavidromeFiles has no entry for song 103, so a stream of it is "the file is
// gone from the server" even though the track is in the index.
func NavidromeFiles() map[string][]byte {
	return map[string][]byte{
		"101":            []byte("0123456789abcdefghijklmnopqrstuvwxyz"),
		"102":            []byte("mp3-bytes"),
		NavidromeMixedID: []byte("dashed-bytes"),
	}
}

func NavidromeArt() map[string][]byte {
	return map[string][]byte{
		"art-101":    []byte("jpeg:101"),
		"art-102":    []byte("jpeg:102"),
		"art-103":    []byte("jpeg:103"),
		"art-105":    []byte("jpeg:105"),
		"art-dashed": []byte("jpeg:dashed"),
	}
}
