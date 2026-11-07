package server

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/navidrome"
)

func TestStreamDescriptors(t *testing.T) {
	h := newHarness(t)
	cases := map[string]struct {
		id   string
		want map[string]any
	}{
		"hi-res flac": {"101", map[string]any{
			"url": testBase + "/file/101", "format": "flac", "quality": "lossless 24-bit 48kHz", "codec": "flac",
			"container": "flac", "manifest": "none", "sampleRate": float64(48000), "bitDepth": float64(24), "bitrate": float64(1875000),
		}},
		"mp3 omits bit depth": {"102", map[string]any{
			"url": testBase + "/file/102", "format": "mp3", "quality": "320kbps", "codec": "mp3",
			"container": "mp3", "manifest": "none", "sampleRate": float64(44100), "bitrate": float64(320000),
		}},
		"wav is lossless": {"103", map[string]any{
			"url": testBase + "/file/103", "format": "wav", "quality": "lossless 16-bit 44.1kHz", "codec": "wav",
			"container": "wav", "manifest": "none", "sampleRate": float64(44100), "bitDepth": float64(16), "bitrate": float64(1411000),
		}},
		"no stream details": {"104", map[string]any{
			"url": testBase + "/file/104", "format": "mp3", "quality": "251kbps", "codec": "mp3",
			"container": "mp3", "manifest": "none", "bitrate": float64(251000),
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rec := h.get("/navidrome-1/" + testSecret + "/stream/" + tc.id + "?quality=HIGH")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}
			if got := decode[map[string]any](t, rec); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("descriptor = %v", got)
			}
		})
	}
}

func TestStreamAnswersFromTheIndexAndAsksTheServerOnlyOnAMiss(t *testing.T) {
	h := newHarness(t)
	before := len(h.fake.Requests())
	if rec := h.get("/navidrome-1/" + testSecret + "/stream/101"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(h.fake.Requests()) != before {
		t.Error("an indexed track must not cost an upstream request")
	}
	cold := newHarnessWith(t, navidrome.Options{}, false)
	if rec := cold.get("/navidrome-1/" + testSecret + "/stream/101"); rec.Code != http.StatusOK {
		t.Fatalf("index miss: status = %d", rec.Code)
	}
	if requests := cold.fake.Requests(); requests[len(requests)-1].Path != "/rest/getSong.view" {
		t.Errorf("index miss did not ask upstream: %v", requests)
	}
}

func TestStreamMisses(t *testing.T) {
	h := newHarness(t)
	for name, id := range map[string]string{"unknown": "999", "nonexistent track": "106"} {
		if rec := h.get("/navidrome-1/" + testSecret + "/stream/" + id); rec.Code != http.StatusNotFound || rec.Body.Len() != 0 {
			t.Errorf("%s: status %d, body %q", name, rec.Code, rec.Body.String())
		}
	}
	before := len(h.fake.Requests())
	h.get("/navidrome-1/" + testSecret + "/stream/1.2")
	if len(h.fake.Requests()) != before {
		t.Error("a malformed id must not reach the server")
	}
}

func TestStreamAnswers502WhenTheServerFails(t *testing.T) {
	t.Run("server 500", func(t *testing.T) {
		h := newHarness(t)
		h.fake.FailWith(http.StatusInternalServerError)
		if rec := h.get("/navidrome-1/" + testSecret + "/stream/999"); rec.Code != http.StatusBadGateway {
			t.Fatalf("status = %d", rec.Code)
		}
	})
	t.Run("rejected credentials are named in the log", func(t *testing.T) {
		h := newHarnessWith(t, navidrome.Options{Password: "wrong"}, false)
		rec := h.get("/navidrome-1/" + testSecret + "/stream/101")
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status = %d", rec.Code)
		}
		if logs := h.logs.String(); !strings.Contains(logs, "navidrome rejected the username or password") {
			t.Fatalf("logs = %s", logs)
		}
	})
	t.Run("a slow server hits the lookup timeout", func(t *testing.T) {
		h := newHarness(t)
		h.fake.Extra["getSong"] = func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
		}
		h.server.lookupTimeout = 50 * time.Millisecond
		started := time.Now()
		rec := h.get("/navidrome-1/" + testSecret + "/stream/999")
		if rec.Code != http.StatusBadGateway || time.Since(started) > time.Second {
			t.Fatalf("status %d after %v", rec.Code, time.Since(started))
		}
	})
}

func TestStreamLogsTheTrackBitChordChose(t *testing.T) {
	h := newHarness(t)
	h.get("/navidrome-1/" + testSecret + "/stream/101")
	want := `"msg":"stream","server":"navidrome-1","source":"navidrome","id":"101","track":"New Religion — All Time Low, Teddy Swims","quality":"lossless 24-bit 48kHz"`
	if logs := h.logs.String(); !strings.Contains(logs, want) {
		t.Fatalf("logs lack %s:\n%s", want, logs)
	}
}
