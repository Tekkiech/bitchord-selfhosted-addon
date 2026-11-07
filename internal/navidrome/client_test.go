package navidrome_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/fakes"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/media"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/navidrome"
)

func newClient(f *fakes.Navidrome) *navidrome.Client {
	return navidrome.New(navidrome.Options{BaseURL: f.URL, Username: fakes.NavidromeUser, Password: fakes.NavidromePassword, PageSize: 2})
}

func TestNameAndVersion(t *testing.T) {
	f := fakes.NewNavidrome(t)
	c := newClient(f)
	if c.Name() != "Navidrome" {
		t.Errorf("Name() = %q", c.Name())
	}
	version, err := c.Version(context.Background())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if version != "0.64.2" {
		t.Errorf("Version() = %q, want the serverVersion", version)
	}
}

func TestLibraries(t *testing.T) {
	f := fakes.NewNavidrome(t)
	libraries, err := newClient(f).Libraries(context.Background())
	if err != nil {
		t.Fatalf("Libraries: %v", err)
	}
	if len(libraries) != 2 || libraries[0].ID != "1" || libraries[0].Name != "Music" || libraries[1].Name != "Audiobooks" {
		t.Fatalf("Libraries() = %+v", libraries)
	}
}

func TestWrongPasswordIsUnauthorized(t *testing.T) {
	f := fakes.NewNavidrome(t)
	c := navidrome.New(navidrome.Options{BaseURL: f.URL, Username: fakes.NavidromeUser, Password: "nope"})
	_, err := c.Libraries(context.Background())
	if !errors.Is(err, media.ErrUnauthorized) {
		t.Fatalf("Libraries with a wrong password = %v, want ErrUnauthorized", err)
	}
}

func TestAllTracksPagesEveryFolder(t *testing.T) {
	f := fakes.NewNavidrome(t)
	tracks, err := newClient(f).AllTracks(context.Background(), "")
	if err != nil {
		t.Fatalf("AllTracks: %v", err)
	}
	// Six songs in Music plus one in Audiobooks, gathered across pages of two.
	if len(tracks) != 7 {
		t.Fatalf("AllTracks() returned %d tracks, want 7", len(tracks))
	}
	ids := map[string]bool{}
	for _, track := range tracks {
		ids[track.ID] = true
	}
	for _, want := range []string{"101", "102", "103", "104", "105", fakes.NavidromeMixedID, "501"} {
		if !ids[want] {
			t.Errorf("AllTracks() missing %q", want)
		}
	}
}

func TestAllTracksFilterByLibrary(t *testing.T) {
	f := fakes.NewNavidrome(t)
	c := newClient(f)
	byName, err := c.AllTracks(context.Background(), "Music")
	if err != nil {
		t.Fatalf("AllTracks(Music): %v", err)
	}
	byID, err := c.AllTracks(context.Background(), "1")
	if err != nil {
		t.Fatalf("AllTracks(1): %v", err)
	}
	if len(byName) != 6 || len(byID) != 6 {
		t.Fatalf("by name %d, by id %d, want 6 each", len(byName), len(byID))
	}
	if _, err := c.AllTracks(context.Background(), "Nope"); err == nil {
		t.Error("AllTracks with an unknown library succeeded")
	}
}

// IgnorePaging makes the fake answer the whole collection to every page; the
// adapter must still stop instead of looping on the repeated first id.
func TestAllTracksStopsWhenServerIgnoresPaging(t *testing.T) {
	f := fakes.NewNavidrome(t)
	f.IgnorePaging = true
	tracks, err := newClient(f).AllTracks(context.Background(), "Music")
	if err != nil {
		t.Fatalf("AllTracks: %v", err)
	}
	if len(tracks) != 6 {
		t.Fatalf("AllTracks() = %d tracks, want 6 with paging ignored", len(tracks))
	}
}

func TestConvertMapsAFlacTrack(t *testing.T) {
	f := fakes.NewNavidrome(t)
	track, err := newClient(f).Track(context.Background(), "101")
	if err != nil {
		t.Fatalf("Track: %v", err)
	}
	want := media.Track{
		ID: "101", Title: "New Religion", Artist: "All Time Low, Teddy Swims",
		AlbumArtist: "All Time Low, Teddy Swims", Album: "Tell Me I’m Alive",
		DurationSec: 184, Codec: "flac", Container: "flac", BitrateKbps: 1875,
		SampleRate: 48000, BitDepth: 24,
	}
	// FileRef and ArtRef are opaque paths; check them apart from the metadata.
	if track.FileRef == "" || !strings.Contains(track.FileRef, "format=raw") || !strings.Contains(track.FileRef, "id=101") {
		t.Errorf("FileRef = %q", track.FileRef)
	}
	if track.ArtRef == "" || !strings.Contains(track.ArtRef, "art-101") {
		t.Errorf("ArtRef = %q", track.ArtRef)
	}
	track.FileRef, track.ArtRef = "", ""
	if track != want {
		t.Fatalf("Track(101) = %+v\nwant %+v", track, want)
	}
	if !media.Lossless(track.Codec) {
		t.Error("a flac track is not reported as lossless")
	}
}

func TestTrackNotFound(t *testing.T) {
	f := fakes.NewNavidrome(t)
	_, err := newClient(f).Track(context.Background(), "does-not-exist")
	if !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("Track(unknown) = %v, want ErrNotFound", err)
	}
}

func TestOpenFileForwardsRange(t *testing.T) {
	f := fakes.NewNavidrome(t)
	c := newClient(f)
	track, err := c.Track(context.Background(), "101")
	if err != nil {
		t.Fatalf("Track: %v", err)
	}
	header := http.Header{"Range": {"bytes=0-3"}}
	res, err := c.OpenFile(context.Background(), track, http.MethodGet, header)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", res.StatusCode)
	}
	if got := res.Header.Get("Content-Range"); !strings.HasPrefix(got, "bytes 0-3/") {
		t.Errorf("Content-Range = %q", got)
	}
	body, _ := io.ReadAll(res.Body)
	if string(body) != "0123" {
		t.Errorf("body = %q, want the first four bytes", body)
	}
}

func TestOpenArt(t *testing.T) {
	f := fakes.NewNavidrome(t)
	c := newClient(f)
	track, err := c.Track(context.Background(), "101")
	if err != nil {
		t.Fatalf("Track: %v", err)
	}
	res, err := c.OpenArt(context.Background(), track)
	if err != nil {
		t.Fatalf("OpenArt: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "image/jpeg" {
		t.Fatalf("status %d, type %q", res.StatusCode, res.Header.Get("Content-Type"))
	}
}

// A Subsonic server answers 200 OK even when the request failed, with the
// failure in the body; the adapter must surface it as an error.
func TestErrorInsideTwoHundred(t *testing.T) {
	f := fakes.NewNavidrome(t)
	f.Extra["getMusicFolders"] = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"subsonic-response":{"status":"failed","version":"1.16.1","error":{"code":0,"message":"boom"}}}`))
	}
	_, err := newClient(f).Libraries(context.Background())
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Libraries = %v, want the server's error", err)
	}
}
