package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/fakes"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/navidrome"
)

const flacBody = "0123456789abcdefghijklmnopqrstuvwxyz"

func filePath(id string) string { return "/navidrome-1/" + testSecret + "/file/" + id }

func TestFileWithoutRange(t *testing.T) {
	rec := newHarness(t).get(filePath("101"))
	if rec.Code != http.StatusOK || rec.Body.String() != flacBody {
		t.Fatalf("status %d, body %q", rec.Code, rec.Body.String())
	}
	want := map[string]string{
		"Content-Type": "audio/flac", "Content-Length": "36", "Accept-Ranges": "bytes",
		"Etag": `"fake-etag"`, "Last-Modified": "Fri, 02 Jan 2026 03:04:05 GMT",
	}
	for name, value := range want {
		if got := rec.Header().Get(name); got != value {
			t.Errorf("%s = %q, want %q", name, got, value)
		}
	}
}

func TestFileWithRange(t *testing.T) {
	rec := newHarness(t).do(http.MethodGet, filePath("101"), http.Header{"Range": {"bytes=10-19"}})
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "abcdefghij" {
		t.Fatalf("status %d, body %q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes 10-19/36" {
		t.Errorf("Content-Range = %q", got)
	}
}

func TestFileRangePastTheEndIsPassedThrough(t *testing.T) {
	rec := newHarness(t).do(http.MethodGet, filePath("101"), http.Header{"Range": {"bytes=100-"}})
	if rec.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestFileHead(t *testing.T) {
	h := newHarness(t)
	rec := h.do(http.MethodHead, filePath("101"), nil)
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 || rec.Header().Get("Content-Length") != "36" {
		t.Fatalf("status %d, body %q, length %q", rec.Code, rec.Body.String(), rec.Header().Get("Content-Length"))
	}
	requests := h.fake.Requests()
	if last := requests[len(requests)-1]; last.Method != http.MethodHead {
		t.Errorf("upstream method = %s", last.Method)
	}
}

func TestFileForwardsOnlyWhitelistedHeaders(t *testing.T) {
	h := newHarness(t)
	rec := h.do(http.MethodGet, filePath("101"), http.Header{
		"Range": {"bytes=0-3"}, "If-Range": {`"fake-etag"`}, "Cookie": {"session=1"}, "Authorization": {"Bearer x"},
	})
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d", rec.Code)
	}
	requests := h.fake.Requests()
	upstream := requests[len(requests)-1].Header
	if upstream.Get("Range") != "bytes=0-3" || upstream.Get("If-Range") != `"fake-etag"` {
		t.Errorf("range headers not forwarded: %v", upstream)
	}
	if upstream.Get("Cookie") != "" || upstream.Get("Authorization") != "" {
		t.Errorf("private request headers reached Navidrome: %v", upstream)
	}
	if upstream.Get("Accept-Encoding") != "identity" {
		t.Errorf("Accept-Encoding = %q", upstream.Get("Accept-Encoding"))
	}
	if rec.Header().Get("X-Navidrome-Fake") != "" {
		t.Error("a Navidrome response header outside the whitelist reached the client")
	}
	for name, values := range rec.Header() {
		if strings.Contains(strings.Join(values, ","), fakes.NavidromePassword) {
			t.Errorf("password leaked in response header %s", name)
		}
	}
}

func TestFileFallsBackToTheServerOnAnIndexMiss(t *testing.T) {
	h := newHarnessWith(t, navidrome.Options{}, false)
	if rec := h.get(filePath("101")); rec.Code != http.StatusOK || rec.Body.String() != flacBody {
		t.Fatalf("status %d, body %q", rec.Code, rec.Body.String())
	}
}

func TestFileMissesAndFailures(t *testing.T) {
	h := newHarness(t)
	for name, id := range map[string]string{"unknown id": "999", "file gone from the server": "103", "unknown short id": "1x", "nonexistent track": "106"} {
		if rec := h.get(filePath(id)); rec.Code != http.StatusNotFound || rec.Body.Len() != 0 {
			t.Errorf("%s: status %d", name, rec.Code)
		}
	}
	h.fake.FailWith(http.StatusInternalServerError)
	if rec := h.get(filePath("101")); rec.Code != http.StatusBadGateway {
		t.Errorf("server 500: status %d", rec.Code)
	}
	if logs := h.logs.String(); !strings.Contains(logs, `"msg":"upstream answered the byte request badly","server":"navidrome-1","source":"navidrome","status":500`) {
		t.Errorf("logs lack the bad-answer line:\n%s", logs)
	}
}

func TestFileAnswers502WhenTheCredentialsAreRejected(t *testing.T) {
	h := newHarnessWith(t, navidrome.Options{Password: "wrong"}, false)
	rec := h.get(filePath("101"))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", rec.Code)
	}
	if logs := h.logs.String(); !strings.Contains(logs, "navidrome rejected the username or password") {
		t.Fatalf("logs = %s", logs)
	}
}

func TestClientDisconnectCancelsTheUpstreamRequest(t *testing.T) {
	h := newHarness(t)
	cancelled := make(chan struct{})
	h.fake.Extra["stream"] = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		for {
			select {
			case <-r.Context().Done():
				close(cancelled)
				return
			default:
				w.Write([]byte("chunk"))
				w.(http.Flusher).Flush()
				time.Sleep(time.Millisecond)
			}
		}
	}
	front := httptest.NewServer(h.handler)
	defer front.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, front.URL+filePath("102"), nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(res.Body, make([]byte, 10)); err != nil {
		t.Fatal(err)
	}
	cancel()
	res.Body.Close()

	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream request was still running 5s after the client left")
	}
}

func TestClientDisconnectBeforeTheServerAnswersCancelsTheUpstreamRequest(t *testing.T) {
	h := newHarness(t)
	reached := make(chan struct{})
	cancelled := make(chan struct{})
	h.fake.Extra["stream"] = func(w http.ResponseWriter, r *http.Request) {
		close(reached)
		<-r.Context().Done()
		close(cancelled)
	}
	front := httptest.NewServer(h.handler)
	defer front.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, front.URL+filePath("102"), nil)
	go http.DefaultClient.Do(req)
	<-reached
	cancel()

	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream request was still running 5s after the client left")
	}
}

func TestABodyLongerThanEveryTimeoutStillCompletes(t *testing.T) {
	h := newHarnessWith(t, navidrome.Options{HeaderTimeout: 50 * time.Millisecond, CallTimeout: 50 * time.Millisecond}, true)
	h.server.lookupTimeout = 50 * time.Millisecond
	h.fake.Extra["stream"] = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		for range 6 {
			w.Write([]byte("slow!"))
			w.(http.Flusher).Flush()
			time.Sleep(40 * time.Millisecond)
		}
	}
	front := httptest.NewServer(h.handler)
	defer front.Close()
	res, err := http.Get(front.URL + filePath("102"))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil || string(body) != strings.Repeat("slow!", 6) {
		t.Fatalf("body %q, err %v", body, err)
	}
}

func TestStreamEndingEarlyIsLoggedAtDebugNotAboveIt(t *testing.T) {
	h := newHarness(t)
	h.fake.Extra["stream"] = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("short"))
		w.(http.Flusher).Flush()
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Fatal(err)
		}
		conn.Close()
	}
	rec := h.get(filePath("102"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	logs := h.logs.String()
	if !strings.Contains(logs, `"msg":"stream ended early"`) || !strings.Contains(logs, `"error":"unexpected EOF"`) {
		t.Fatalf("logs lack the early-stream-end message:\n%s", logs)
	}
	if strings.Contains(logs, `"level":"ERROR"`) || strings.Contains(logs, `"level":"WARN"`) || strings.Contains(logs, `"level":"INFO"`) {
		for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
			if strings.Contains(line, "stream ended early") && !strings.Contains(line, `"level":"DEBUG"`) {
				t.Fatalf("stream ended early logged above debug: %s", line)
			}
		}
	}
}

func TestArt(t *testing.T) {
	h := newHarness(t)
	rec := h.get("/navidrome-1/" + testSecret + "/art/101")
	if rec.Code != http.StatusOK || rec.Body.String() != "jpeg:101" {
		t.Fatalf("status %d, body %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Content-Type") != "image/jpeg" || rec.Header().Get("Cache-Control") != "public, max-age=86400" {
		t.Errorf("headers = %v", rec.Header())
	}
	requests := h.fake.Requests()
	last := requests[len(requests)-1]
	if last.Path != "/rest/getCoverArt.view" || !strings.Contains(last.Query, "id=art-101") || !strings.Contains(last.Query, "size=600") {
		t.Errorf("upstream = %s?%s", last.Path, last.Query)
	}
	if strings.Contains(last.Query, "p=") || strings.Contains(last.Query, fakes.NavidromePassword) {
		t.Error("the plaintext password reached the artwork URL")
	}
}

func TestArtMisses(t *testing.T) {
	h := newHarness(t)
	for name, id := range map[string]string{"track without cover art": "104", "unknown id": "999"} {
		if rec := h.get("/navidrome-1/" + testSecret + "/art/" + id); rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d", name, rec.Code)
		}
	}
	cold := newHarnessWith(t, navidrome.Options{}, false)
	if rec := cold.get("/navidrome-1/" + testSecret + "/art/101"); rec.Code != http.StatusOK {
		t.Errorf("index miss: status %d", rec.Code)
	}
	head := h.do(http.MethodHead, "/navidrome-1/"+testSecret+"/art/101", nil)
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Errorf("HEAD: status %d, body %q", head.Code, head.Body.String())
	}
}

func TestPlaysAreLogged(t *testing.T) {
	h := newHarness(t)
	h.get(filePath("101"))
	h.do(http.MethodGet, filePath("101"), http.Header{"Range": {"bytes=10-19"}})
	h.do(http.MethodHead, filePath("101"), nil)
	logs := h.logs.String()
	for _, want := range []string{
		`"level":"INFO","msg":"play","server":"navidrome-1","source":"navidrome","id":"101","track":"New Religion — All Time Low, Teddy Swims","range":"","status":200,"bytes":36,"ended":"complete"`,
		`"msg":"play","server":"navidrome-1","source":"navidrome","id":"101","track":"New Religion — All Time Low, Teddy Swims","range":"bytes=10-19","status":206,"bytes":10,"ended":"complete"`,
		`"level":"DEBUG","msg":"probe","server":"navidrome-1","source":"navidrome","id":"101"`,
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs lack %s:\n%s", want, logs)
		}
	}
}

func TestAPlayTheClientAbandonsIsLoggedAsSuch(t *testing.T) {
	h := newHarness(t)
	h.fake.Extra["stream"] = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		for r.Context().Err() == nil {
			w.Write([]byte("chunk"))
			w.(http.Flusher).Flush()
			time.Sleep(time.Millisecond)
		}
	}
	front := httptest.NewServer(h.handler)
	defer front.Close()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, front.URL+filePath("102"), nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.ReadFull(res.Body, make([]byte, 10))
	cancel()
	res.Body.Close()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(h.logs.String(), `"ended":"client left"`) {
		if time.Now().After(deadline) {
			t.Fatalf("no abandoned play in the logs:\n%s", h.logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
