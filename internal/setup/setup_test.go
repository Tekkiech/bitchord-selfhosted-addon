package setup

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/fakes"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/registry"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/store"
)

const password = "correct horse battery staple"

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type testApp struct {
	t       *testing.T
	handler http.Handler
	setup   *Handler
	store   *store.Store
	reg     *registry.Registry
	logs    *syncBuffer
	log     *slog.Logger
	now     time.Time
	cookie  string
	headers http.Header
}

func newTestApp(t *testing.T) *testApp {
	t.Helper()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	a := &testApp{t: t, store: s, logs: logs, log: log, now: time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC), headers: http.Header{}}
	a.reg = registry.New(s.Snapshot(), registry.Options{Interval: time.Hour, Version: "1.2.3", Log: log, ProbeTimeout: time.Second})
	t.Cleanup(a.reg.Stop)
	a.setup = New(Options{Store: s, Registry: a.reg, Version: "1.2.3", Log: log, Now: func() time.Time { return a.now }})
	t.Cleanup(a.setup.Close)
	a.handler = a.setup
	return a
}

func (a *testApp) do(method, path string, body io.Reader, contentType string) *httptest.ResponseRecorder {
	a.t.Helper()
	req := httptest.NewRequest(method, path, body)
	req.RemoteAddr = "10.0.0.7:4444"
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for key, values := range a.headers {
		req.Header[key] = values
	}
	if a.cookie != "" {
		req.AddCookie(&http.Cookie{Name: cookieName, Value: a.cookie})
	}
	rec := httptest.NewRecorder()
	a.handler.ServeHTTP(rec, req)
	if cookie := sessionCookie(rec); cookie != nil {
		a.cookie = cookie.Value
	}
	return rec
}

func sessionCookie(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == cookieName {
			return cookie
		}
	}
	return nil
}

func (a *testApp) get(path string) *httptest.ResponseRecorder {
	return a.do(http.MethodGet, path, nil, "")
}

func (a *testApp) csrf() string {
	return csrfToken(sessionKey(a.store.Snapshot().Admin), a.cookie)
}

func (a *testApp) form(path string, values url.Values) *httptest.ResponseRecorder {
	if a.cookie != "" && values.Get("csrf") == "" {
		values.Set("csrf", a.csrf())
	}
	return a.do(http.MethodPost, path, strings.NewReader(values.Encode()), "application/x-www-form-urlencoded")
}

func (a *testApp) json(method, path string, body any) (*httptest.ResponseRecorder, map[string]any) {
	a.t.Helper()
	raw, _ := json.Marshal(body)
	a.headers.Set("X-CSRF-Token", a.csrf())
	defer a.headers.Del("X-CSRF-Token")
	rec := a.do(method, path, bytes.NewReader(raw), "application/json")
	var answer map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
		a.t.Fatalf("%s %s: body is not JSON: %s", method, path, rec.Body.String())
	}
	return rec, answer
}

func (a *testApp) signIn() {
	a.t.Helper()
	if rec := a.form("/setup/password", url.Values{"password": {password}}); rec.Code != http.StatusSeeOther || a.cookie == "" {
		a.t.Fatalf("create password: status %d, cookie %q", rec.Code, a.cookie)
	}
}

// addServer saves a Navidrome server through the form and returns its slug.
func (a *testApp) addServer(fake *fakes.Navidrome, label string) string {
	a.t.Helper()
	rec := a.form("/setup/servers", url.Values{
		"label": {label}, "url": {fake.URL}, "username": {fakes.NavidromeUser},
		"password": {fakes.NavidromePassword}, "library": {"Music"}, "enabled": {"1"},
	})
	if rec.Code != http.StatusSeeOther {
		a.t.Fatalf("add server: status %d\n%s", rec.Code, rec.Body.String())
	}
	servers := a.store.Snapshot().Servers
	return servers[len(servers)-1].Slug
}

// ---- first visit and password

func TestWithoutAnAdminOnlyTheCreateFormExists(t *testing.T) {
	a := newTestApp(t)
	first := a.get("/setup")
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), "Create a password") {
		t.Fatalf("first visit: %d %s", first.Code, first.Body.String())
	}
	for _, path := range []string{"/setup/servers/new", "/setup/login", "/setup/password"} {
		if rec := a.get(path); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/setup" {
			t.Errorf("GET %s: %d -> %q", path, rec.Code, rec.Header().Get("Location"))
		}
	}
	rec := a.form("/setup/servers", url.Values{"url": {"http://navidrome:4533"}, "username": {"lyle"}, "password": {"x"}})
	if rec.Code != http.StatusSeeOther || len(a.store.Snapshot().Servers) != 0 {
		t.Fatalf("POST without an admin: %d, servers %v", rec.Code, a.store.Snapshot().Servers)
	}
	short := a.form("/setup/password", url.Values{"password": {"short"}})
	if short.Code != http.StatusBadRequest || !strings.Contains(short.Body.String(), "at least 12 characters") || a.store.Snapshot().Admin != nil {
		t.Fatalf("short password: %d", short.Code)
	}
	a.signIn()
	admin := a.store.Snapshot().Admin
	if admin == nil || !VerifyPassword(admin.PasswordHash, password) || admin.SessionKey == "" {
		t.Fatalf("admin = %+v", admin)
	}
	if page := a.get("/setup").Body.String(); strings.Contains(page, `id="secret"`) || strings.Contains(page, "Add a server") {
		t.Fatal("the secret or Add a server shows before a public URL is set")
	}
	if err := a.store.SetPublicURL("https://music.example.com"); err != nil {
		t.Fatal(err)
	}
	if page := a.get("/setup").Body.String(); !strings.Contains(page, `id="secret"`) || !strings.Contains(page, "No servers yet") {
		t.Fatalf("overview with a public URL: %s", page)
	}
	if strings.Contains(a.logs.String(), password) {
		t.Fatal("the password reached the logs")
	}
}

func TestLoginLockoutAndCookieFlags(t *testing.T) {
	a := newTestApp(t)
	a.signIn()
	a.cookie = ""
	if rec := a.get("/setup"); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/setup/login" {
		t.Fatalf("no session: %d -> %q", rec.Code, rec.Header().Get("Location"))
	}
	if rec := a.form("/setup/servers/navidrome-1/delete", url.Values{}); rec.Code != http.StatusSeeOther {
		t.Fatalf("POST without a session: %d", rec.Code)
	}
	for i := range 5 {
		if rec := a.form("/setup/login", url.Values{"password": {"wrong"}}); rec.Code != http.StatusUnauthorized || a.cookie != "" {
			t.Fatalf("attempt %d: %d, cookie %q", i, rec.Code, a.cookie)
		}
	}
	if rec := a.form("/setup/login", url.Values{"password": {password}}); rec.Code != http.StatusTooManyRequests || a.cookie != "" {
		t.Fatalf("locked out login: %d", rec.Code)
	}
	a.now = a.now.Add(61 * time.Second)
	rec := a.form("/setup/login", url.Values{"password": {password}})
	if rec.Code != http.StatusSeeOther || a.cookie == "" {
		t.Fatalf("login after lockout: %d", rec.Code)
	}
	cookie := sessionCookie(rec)
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/setup" || cookie.Secure {
		t.Fatalf("cookie over http = %+v", cookie)
	}
	a.cookie = ""
	a.headers.Set("X-Forwarded-Proto", "https")
	secure := sessionCookie(a.form("/setup/login", url.Values{"password": {password}}))
	if secure == nil || !secure.Secure {
		t.Fatalf("cookie over https = %+v", secure)
	}
	a.headers.Del("X-Forwarded-Proto")
	if logs := a.logs.String(); !strings.Contains(logs, `"msg":"setup login failed","from":"10.0.0.7"`) || strings.Contains(logs, "wrong") {
		t.Fatalf("logs = %s", logs)
	}
	a.now = a.now.Add(SessionLifetime)
	if rec := a.get("/setup"); rec.Code != http.StatusSeeOther {
		t.Fatalf("expired session still valid: %d", rec.Code)
	}
}

func TestCSRFAndCrossSitePostsAreRefused(t *testing.T) {
	a := newTestApp(t)
	a.signIn()
	attempts := map[string]string{"missing": "public_url=https%3A%2F%2Fmusic.example.com", "wrong": "public_url=https%3A%2F%2Fmusic.example.com&csrf=nope"}
	for name, body := range attempts {
		rec := a.do(http.MethodPost, "/setup/public-url", strings.NewReader(body), "application/x-www-form-urlencoded")
		if rec.Code != http.StatusForbidden || a.store.Snapshot().PublicURL != "" {
			t.Errorf("%s csrf: %d, public url %q", name, rec.Code, a.store.Snapshot().PublicURL)
		}
	}
	a.headers.Set("Sec-Fetch-Site", "cross-site")
	if rec := a.form("/setup/public-url", url.Values{"public_url": {"https://music.example.com"}}); rec.Code != http.StatusForbidden {
		t.Errorf("cross-site: %d", rec.Code)
	}
	a.headers.Set("Sec-Fetch-Site", "same-origin")
	if rec := a.form("/setup/public-url", url.Values{"public_url": {"https://music.example.com"}}); rec.Code != http.StatusSeeOther {
		t.Errorf("same-origin: %d", rec.Code)
	}
	a.headers.Set("Origin", "https://evil.example")
	if rec := a.form("/setup/public-url", url.Values{"public_url": {"https://music.example.com"}}); rec.Code != http.StatusSeeOther {
		t.Errorf("sec-fetch-site must win over origin, proxies may rewrite the host: %d", rec.Code)
	}
	a.headers.Del("Sec-Fetch-Site")
	if rec := a.form("/setup/public-url", url.Values{"public_url": {"https://music.example.com"}}); rec.Code != http.StatusForbidden {
		t.Errorf("foreign origin without sec-fetch-site: %d", rec.Code)
	}
	a.headers.Set("Origin", "http://example.com")
	if rec := a.form("/setup/public-url", url.Values{"public_url": {"https://music.example.com"}}); rec.Code != http.StatusSeeOther {
		t.Errorf("matching origin without sec-fetch-site: %d", rec.Code)
	}
	a.headers.Del("Origin")
}

func TestChangePasswordSignsOtherBrowsersOut(t *testing.T) {
	a := newTestApp(t)
	a.signIn()
	other := a.cookie
	if rec := a.form("/setup/password", url.Values{"current": {"wrong"}, "password": {"another long password"}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("wrong current password: %d", rec.Code)
	}
	if rec := a.form("/setup/password", url.Values{"current": {password}, "password": {"another long password"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("change: %d %s", rec.Code, rec.Body.String())
	}
	if rec := a.get("/setup"); rec.Code != http.StatusOK {
		t.Fatalf("new session: %d", rec.Code)
	}
	a.cookie = other
	if rec := a.get("/setup"); rec.Code != http.StatusSeeOther {
		t.Fatalf("old session still valid: %d", rec.Code)
	}
	a.cookie = ""
	if rec := a.form("/setup/login", url.Values{"password": {"another long password"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("login with the new password: %d", rec.Code)
	}
	if rec := a.form("/setup/logout", url.Values{}); rec.Code != http.StatusSeeOther || sessionCookie(rec).MaxAge != -1 {
		t.Fatalf("logout: %d", rec.Code)
	}
}

// ---- public url, secret, servers

func TestPublicURLAndSecret(t *testing.T) {
	a := newTestApp(t)
	a.signIn()
	if rec := a.get("/setup"); !strings.Contains(rec.Body.String(), "Server URLs appear once it is set") {
		t.Fatal("missing public url banner")
	}
	if rec := a.form("/setup/public-url", url.Values{"public_url": {"http://music.example.com"}}); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "https://") {
		t.Fatalf("http public url: %d", rec.Code)
	}
	if rec := a.form("/setup/public-url", url.Values{"public_url": {"https://music.example.com/"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("public url: %d", rec.Code)
	}
	fake := fakes.NewNavidrome(t)
	slug := a.addServer(fake, "Home")
	before := a.store.Snapshot().Secret
	page := a.get("/setup").Body.String()
	if !strings.Contains(page, "https://music.example.com/"+slug+"/"+before) || !strings.Contains(page, "Navidrome - Home") {
		t.Fatalf("overview lacks the server url:\n%s", page)
	}
	if rec := a.form("/setup/secret/regenerate", url.Values{}); rec.Code != http.StatusSeeOther {
		t.Fatalf("regenerate: %d", rec.Code)
	}
	after := a.store.Snapshot().Secret
	page = a.get("/setup").Body.String()
	if after == before || strings.Contains(page, before) || !strings.Contains(page, "https://music.example.com/"+slug+"/"+after) {
		t.Fatalf("secret not rotated on the page")
	}
	if rec := a.form("/setup/public-url", url.Values{"public_url": {""}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("clear public url: %d", rec.Code)
	}
	if got := a.store.Snapshot().PublicURL; got != "" {
		t.Fatalf("public url after clearing = %q", got)
	}
	if page = a.get("/setup").Body.String(); !strings.Contains(page, "Server URLs appear once it is set") {
		t.Fatal("missing public url banner after clearing")
	}
}

func TestServerLifecycleThroughTheForms(t *testing.T) {
	a := newTestApp(t)
	a.signIn()
	a.form("/setup/public-url", url.Values{"public_url": {"https://music.example.com"}})
	fake := fakes.NewNavidrome(t)
	if rec := a.get("/setup/servers/new"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `name="username"`) {
		t.Fatalf("new form: %d", rec.Code)
	}
	slug := a.addServer(fake, "Home")
	server, _ := a.store.Snapshot().Server(slug)
	if slug != "navidrome-1" || server.Account != fakes.NavidromeUser || server.Token != fakes.NavidromePassword || server.Auth != store.AuthPassword || server.Library != "1" || server.LibraryName != "Music" || !server.Enabled {
		t.Fatalf("stored = %+v", server)
	}
	deadline := time.Now().Add(5 * time.Second)
	for status, ok := a.reg.Status(slug); !ok || !status.Ready; status, ok = a.reg.Status(slug) {
		if time.Now().After(deadline) {
			t.Fatal("registry never indexed the added server")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if rec := a.get("/setup"); !strings.Contains(rec.Body.String(), "6 tracks") || !strings.Contains(rec.Body.String(), "<dt>Library</dt><dd>Music</dd>") {
		t.Fatalf("overview lacks the track count or the library name:\n%s", rec.Body.String())
	}

	edit := a.get("/setup/servers/navidrome-1/edit")
	if edit.Code != http.StatusOK || !strings.Contains(edit.Body.String(), `value="Home"`) || !strings.Contains(edit.Body.String(), `value="lyle"`) || !strings.Contains(edit.Body.String(), "leave empty to keep") || !strings.Contains(edit.Body.String(), `<option value="1" selected>Music</option>`) {
		t.Fatalf("edit form: %d %s", edit.Code, edit.Body.String())
	}
	a.store.UpdateServer(slug, func(server *store.Server) error {
		server.LibraryName = ""
		return nil
	})
	if !strings.Contains(a.get("/setup").Body.String(), "<dt>Library</dt><dd>1</dd>") || !strings.Contains(a.get("/setup/servers/navidrome-1/edit").Body.String(), `<option value="1" selected>1</option>`) {
		t.Fatal("a server saved without a library name does not fall back to its id")
	}
	rec := a.form("/setup/servers/navidrome-1", url.Values{"label": {"Parents"}, "url": {fake.URL}, "username": {fakes.NavidromeUser}, "library": {""}, "enabled": {"1"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("edit: %d %s", rec.Code, rec.Body.String())
	}
	server, _ = a.store.Snapshot().Server(slug)
	if server.Label != "Parents" || server.Token != fakes.NavidromePassword || server.Library != "" {
		t.Fatalf("after edit = %+v", server)
	}
	if entry, _ := a.reg.Lookup(slug); entry.DisplayName() != "Navidrome - Parents" {
		t.Fatalf("registry name = %q", entry.DisplayName())
	}

	if rec := a.form("/setup/servers/navidrome-1", url.Values{"label": {"x"}, "url": {fake.URL}, "username": {fakes.NavidromeUser}, "library": {"Movies"}, "enabled": {"1"}}); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "no music library") {
		t.Fatalf("unknown library: %d", rec.Code)
	}
	if rec := a.form("/setup/servers/navidrome-1", url.Values{"label": {"x"}, "url": {fake.URL}, "username": {fakes.NavidromeUser}, "password": {"wrong"}, "enabled": {"1"}}); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "rejected the credentials") {
		t.Fatalf("wrong password: %d %s", rec.Code, rec.Body.String())
	}
	if server, _ := a.store.Snapshot().Server(slug); server.Token != fakes.NavidromePassword {
		t.Fatal("a failed save changed the password")
	}

	if rec := a.form("/setup/servers/navidrome-1/enabled", url.Values{"enabled": {"false"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("disable: %d", rec.Code)
	}
	if _, ok := a.reg.Lookup(slug); ok {
		t.Fatal("disabled server still running")
	}
	if server, _ := a.store.Snapshot().Server(slug); server.Enabled {
		t.Fatal("disabled flag not stored")
	}
	second := a.addServer(fakes.NewNavidrome(t), "")
	if second != "navidrome-2" {
		t.Fatalf("second slug = %q", second)
	}
	if rec := a.form("/setup/servers/navidrome-1/delete", url.Values{}); rec.Code != http.StatusSeeOther {
		t.Fatalf("delete: %d", rec.Code)
	}
	if _, ok := a.store.Snapshot().Server("navidrome-1"); ok {
		t.Fatal("deleted server still stored")
	}
	if rec := a.form("/setup/servers/navidrome-9/delete", url.Values{}); rec.Code != http.StatusNotFound {
		t.Fatalf("delete unknown: %d", rec.Code)
	}
	if rec := a.form("/setup/servers/navidrome-9/enabled", url.Values{"enabled": {"false"}}); rec.Code != http.StatusNotFound {
		t.Fatalf("disable unknown: %d", rec.Code)
	}
	if rec := a.get("/setup/servers/navidrome-9/edit"); rec.Code != http.StatusNotFound {
		t.Fatalf("edit unknown: %d", rec.Code)
	}
	if rec := a.form("/setup/servers", url.Values{"url": {fake.URL}, "enabled": {"1"}}); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "a username is required") {
		t.Fatalf("no username: %d", rec.Code)
	}
	if rec := a.form("/setup/servers", url.Values{"url": {fake.URL}, "username": {fakes.NavidromeUser}, "enabled": {"1"}}); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "a password is required") {
		t.Fatalf("no password: %d", rec.Code)
	}
	if strings.Contains(a.logs.String(), fakes.NavidromePassword) {
		t.Fatal("a password reached the logs")
	}
}

func TestTestConnectionEndpoint(t *testing.T) {
	a := newTestApp(t)
	a.signIn()
	fake := fakes.NewNavidrome(t)
	rec, answer := a.json(http.MethodPost, "/setup/servers/test", testRequest{URL: fake.URL, Username: fakes.NavidromeUser, Password: fakes.NavidromePassword})
	if rec.Code != http.StatusOK || answer["ok"] != true || answer["version"] != "0.64.2" {
		t.Fatalf("test: %d %v", rec.Code, answer)
	}
	if libraries := answer["libraries"].([]any); len(libraries) != 2 || libraries[0].(map[string]any)["name"] != "Music" {
		t.Fatalf("libraries = %v", answer["libraries"])
	}
	rec, answer = a.json(http.MethodPost, "/setup/servers/test", testRequest{URL: fake.URL, Username: fakes.NavidromeUser, Password: "wrong"})
	if rec.Code != http.StatusUnauthorized || answer["ok"] != false || !strings.Contains(answer["error"].(string), "rejected the credentials") {
		t.Fatalf("wrong password: %d %s", rec.Code, rec.Body.String())
	}
	rec, answer = a.json(http.MethodPost, "/setup/servers/test", testRequest{URL: fakes.DeadURL(t), Username: fakes.NavidromeUser, Password: "x"})
	if message, _ := answer["error"].(string); rec.Code != http.StatusBadGateway || answer["ok"] != false || !strings.HasPrefix(message, "navidrome:") || strings.Contains(message, "rejected the credentials") {
		t.Fatalf("dead server: %d %v", rec.Code, answer)
	}
	slug := a.addServer(fake, "Home")
	rec, answer = a.json(http.MethodPost, "/setup/servers/test", testRequest{URL: fake.URL, Slug: slug})
	if rec.Code != http.StatusOK || answer["ok"] != true {
		t.Fatalf("test with the stored password: %d %v", rec.Code, answer)
	}
	rec, _ = a.json(http.MethodPost, "/setup/servers/test", testRequest{URL: fake.URL})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("test without a credential: %d", rec.Code)
	}
	a.headers.Set("X-CSRF-Token", "bad")
	if rec := a.do(http.MethodPost, "/setup/servers/test", strings.NewReader("{}"), "application/json"); rec.Code != http.StatusForbidden {
		t.Fatalf("json post with a bad csrf header: %d", rec.Code)
	}
}

func TestTestConnectionLimiterCountsOnlyRejectedCredentials(t *testing.T) {
	a := newTestApp(t)
	a.signIn()
	fake := fakes.NewNavidrome(t)
	fake.FailWith(http.StatusServiceUnavailable)
	for i := range 6 {
		if rec, _ := a.json(http.MethodPost, "/setup/servers/test", testRequest{URL: fake.URL, Username: fakes.NavidromeUser, Password: fakes.NavidromePassword}); rec.Code != http.StatusBadGateway {
			t.Fatalf("upstream failure %d: %d", i, rec.Code)
		}
	}
	fake.FailWith(0)
	for i := range 4 {
		if rec, _ := a.json(http.MethodPost, "/setup/servers/test", testRequest{URL: fake.URL, Username: fakes.NavidromeUser, Password: "nope"}); rec.Code != http.StatusUnauthorized {
			t.Fatalf("rejected %d: %d", i, rec.Code)
		}
	}
	if rec, _ := a.json(http.MethodPost, "/setup/servers/test", testRequest{URL: fake.URL, Username: fakes.NavidromeUser, Password: fakes.NavidromePassword}); rec.Code != http.StatusOK {
		t.Fatalf("test after upstream failures and four rejections: %d", rec.Code)
	}
}

func TestConcurrentLoginAttemptsAreLimitedAtomically(t *testing.T) {
	a := newTestApp(t)
	a.signIn()
	a.cookie = ""

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/setup/login", strings.NewReader(url.Values{"password": {"wrong"}}.Encode()))
			req.RemoteAddr = "10.0.0.9:5555"
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			a.handler.ServeHTTP(httptest.NewRecorder(), req)
		}()
	}
	wg.Wait()

	req := httptest.NewRequest(http.MethodPost, "/setup/login", strings.NewReader(url.Values{"password": {password}}.Encode()))
	req.RemoteAddr = "10.0.0.9:5555"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	a.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("after 20 concurrent attempts: %d", rec.Code)
	}
	if failures := strings.Count(a.logs.String(), `"msg":"setup login failed"`); failures > 5 {
		t.Fatalf("VerifyPassword ran %d times, want at most 5", failures)
	}
}

func TestLoginLockoutIgnoresXForwardedFor(t *testing.T) {
	a := newTestApp(t)
	a.signIn()
	a.cookie = ""
	for i := range 6 {
		a.headers.Set("X-Forwarded-For", fmt.Sprintf("203.0.113.%d", i))
		rec := a.form("/setup/login", url.Values{"password": {"wrong"}})
		switch {
		case i < 5 && rec.Code != http.StatusUnauthorized:
			t.Fatalf("attempt %d: %d", i, rec.Code)
		case i == 5 && rec.Code != http.StatusTooManyRequests:
			t.Fatalf("sixth attempt with rotated X-Forwarded-For: %d", rec.Code)
		}
	}
	a.headers.Del("X-Forwarded-For")
}

func TestStaticAndUnknownSetupPaths(t *testing.T) {
	a := newTestApp(t)
	if rec := a.get("/setup/static/style.css"); rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), "text/css") {
		t.Fatalf("stylesheet: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	a.signIn()
	if rec := a.get("/setup/nope"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown path: %d", rec.Code)
	}
	if rec := a.get("/setup/"); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/setup" {
		t.Fatalf("trailing slash: %d", rec.Code)
	}
	if rec := a.get("/setup"); rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("pages must not be cached")
	}
}

// applyRecorder records every snapshot handed to Apply. Its first Apply holds
// until the store has moved past the snapshot it was given, so a second write
// lands while the first is still applying.
type applyRecorder struct {
	Runtime
	store   *store.Store
	entered chan struct{}
	mu      sync.Mutex
	applied []store.Snapshot
}

func (r *applyRecorder) Apply(snapshot store.Snapshot) {
	r.mu.Lock()
	entered := r.entered
	r.entered = nil
	r.mu.Unlock()
	if entered != nil {
		close(entered)
		deadline := time.Now().Add(2 * time.Second)
		for reflect.DeepEqual(r.store.Snapshot(), snapshot) && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.applied = append(r.applied, snapshot)
}

func (r *applyRecorder) last() store.Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.applied[len(r.applied)-1]
}

func TestConcurrentWritesApplyTheLatestSnapshotLast(t *testing.T) {
	a := newTestApp(t)
	a.signIn()
	for range 2 {
		if _, err := a.store.AddServer(store.Server{Kind: store.Navidrome, URL: "http://navidrome:4533", Account: "lyle", Token: "t", Auth: store.AuthPassword, Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	recorder := &applyRecorder{Runtime: a.reg, store: a.store, entered: make(chan struct{})}
	a.handler = New(Options{Store: a.store, Registry: recorder, Now: func() time.Time { return a.now }})
	disable := func(slug string) *http.Request {
		body := url.Values{"enabled": {"false"}, "csrf": {a.csrf()}}.Encode()
		req := httptest.NewRequest(http.MethodPost, "/setup/servers/"+slug+"/enabled", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(&http.Cookie{Name: cookieName, Value: a.cookie})
		return req
	}
	first, second := disable("navidrome-1"), disable("navidrome-2")
	entered := recorder.entered
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		a.handler.ServeHTTP(httptest.NewRecorder(), first)
	}()
	<-entered
	go func() {
		defer wg.Done()
		a.handler.ServeHTTP(httptest.NewRecorder(), second)
	}()
	wg.Wait()
	if got, want := recorder.last(), a.store.Snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("last applied servers = %+v, store servers = %+v", got.Servers, want.Servers)
	}
}

func TestSignedInLoginPageRedirectsToTheOverview(t *testing.T) {
	a := newTestApp(t)
	a.signIn()
	if rec := a.get("/setup/login"); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/setup" {
		t.Fatalf("GET /setup/login signed in: %d -> %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestSinceDropsZeroUnits(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	cases := map[time.Duration]string{
		30 * time.Second:                            "just now",
		5 * time.Minute:                             "5m ago",
		5*time.Minute + 40*time.Second:              "5m ago",
		2 * time.Hour:                               "2h ago",
		2*time.Hour + 5*time.Minute:                 "2h5m ago",
		2*time.Hour + 5*time.Minute + 9*time.Second: "2h5m ago",
	}
	for ago, want := range cases {
		if got := since(now.Add(-ago), now); got != want {
			t.Errorf("since(%s) = %q, want %q", ago, got, want)
		}
	}
	if got := since(time.Time{}, now); got != "never" {
		t.Errorf("zero time = %q", got)
	}
}

func TestOverHTTPSReadsTheFirstForwardedProto(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/setup", nil)
	r.Header.Set("X-Forwarded-Proto", "https, http")
	if !overHTTPS(r) {
		t.Error("an https first hop was not recognised")
	}
	r.Header.Set("X-Forwarded-Proto", "http, https")
	if overHTTPS(r) {
		t.Error("an http first hop was treated as https")
	}
}

func TestTheFirstPasswordSavesThePublicURLFromTheRequest(t *testing.T) {
	cases := map[string]struct {
		target, proto, want string
	}{
		"https":                 {"https://music.example.com/setup/password", "", "https://music.example.com"},
		"behind a proxy":        {"http://music.example.com/setup/password", "https", "https://music.example.com"},
		"a port is kept":        {"https://music.example.com:8443/setup/password", "", "https://music.example.com:8443"},
		"plain http":            {"http://music.example.com/setup/password", "", ""},
		"localhost":             {"https://localhost/setup/password", "", ""},
		"an ip address":         {"https://192.168.1.10/setup/password", "", ""},
		"an ipv6 address":       {"https://[::1]:8080/setup/password", "", ""},
		"a docker service name": {"http://bitchord-selfhosted-addon:8080/setup/password", "https", ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			a := newTestApp(t)
			if tc.proto != "" {
				a.headers.Set("X-Forwarded-Proto", tc.proto)
			}
			if rec := a.form(tc.target, url.Values{"password": {password}}); rec.Code != http.StatusSeeOther {
				t.Fatalf("create password: %d %s", rec.Code, rec.Body.String())
			}
			if got := a.store.Snapshot().PublicURL; got != tc.want {
				t.Fatalf("public url = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTheFirstPasswordKeepsAPublicURLAlreadySet(t *testing.T) {
	a := newTestApp(t)
	if err := a.store.SetPublicURL("https://other.example.com"); err != nil {
		t.Fatal(err)
	}
	a.form("https://music.example.com/setup/password", url.Values{"password": {password}})
	if got := a.store.Snapshot().PublicURL; got != "https://other.example.com" {
		t.Fatalf("public url = %q", got)
	}
}
