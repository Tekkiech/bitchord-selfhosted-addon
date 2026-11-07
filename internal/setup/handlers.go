package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/media"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/registry"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/store"
)

const probeTimeout = 15 * time.Second

func (a *app) render(w http.ResponseWriter, r *http.Request, status int, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := a.pages[name].ExecuteTemplate(w, "layout.html", data); err != nil {
		a.Log.Error("template failed", "page", name, "error", err.Error())
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}

func jsonError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": message})
}

func readJSON(r *http.Request, into any) error {
	return json.NewDecoder(r.Body).Decode(into)
}

// apply takes the snapshot inside the lock, so a write that saved earlier can
// never apply after one that saved later.
func (a *app) apply() {
	a.applyMu.Lock()
	defer a.applyMu.Unlock()
	a.Registry.Apply(a.Store.Snapshot())
}

func redirectNotice(w http.ResponseWriter, r *http.Request, notice string) {
	http.Redirect(w, r, "/setup?notice="+url.QueryEscape(notice), http.StatusSeeOther)
}

// ---- password and session

func (a *app) createPassword(w http.ResponseWriter, r *http.Request) {
	password := r.PostFormValue("password")
	if utf8.RuneCountInString(password) < MinPasswordLength {
		view := passwordView{page: a.page(r, "Create a password"), Create: true}
		view.Error = fmt.Sprintf("the password must be at least %d characters", MinPasswordLength)
		a.render(w, r, http.StatusBadRequest, "password", view)
		return
	}
	admin := &store.Admin{PasswordHash: HashPassword(password), SessionKey: NewSessionKey()}
	if err := a.Store.SetAdmin(admin); err != nil {
		a.fail(w, r, err)
		return
	}
	a.Log.Info("admin password created", "from", clientAddress(r))
	a.setSessionCookie(w, r, admin)
	if origin, ok := publicOrigin(r); ok && a.Store.Snapshot().PublicURL == "" {
		if err := a.Store.SetPublicURL(origin); err != nil {
			a.fail(w, r, err)
			return
		}
		a.Log.Info("public url set", "public_url", origin)
		redirectNotice(w, r, "Password set. The public URL is "+origin+"; change it below if that is wrong. Now add a server.")
		return
	}
	redirectNotice(w, r, "Password set. Add your public URL and a server.")
}

func (a *app) login(w http.ResponseWriter, r *http.Request, admin *store.Admin) {
	peer := peerAddress(r)
	if !a.logins.attempt(peer) {
		view := loginView{page: a.page(r, "Sign in")}
		view.Error = "too many failed attempts, try again in a minute"
		a.render(w, r, http.StatusTooManyRequests, "login", view)
		return
	}
	if !a.verifyPassword(r.Context(), admin.PasswordHash, r.PostFormValue("password")) {
		a.Log.Warn("setup login failed", "from", clientAddress(r))
		view := loginView{page: a.page(r, "Sign in")}
		view.Error = "wrong password"
		a.render(w, r, http.StatusUnauthorized, "login", view)
		return
	}
	a.logins.reset(peer)
	a.setSessionCookie(w, r, admin)
	http.Redirect(w, r, "/setup", http.StatusSeeOther)
}

// verifyPassword caps concurrent argon2id verifications, each of which
// allocates a fixed amount of memory, so a burst of requests cannot exhaust
// it. It gives up if ctx ends first.
func (a *app) verifyPassword(ctx context.Context, hash, password string) bool {
	select {
	case a.verifySem <- struct{}{}:
	case <-ctx.Done():
		return false
	}
	defer func() { <-a.verifySem }()
	return VerifyPassword(hash, password)
}

func (a *app) logout(w http.ResponseWriter, r *http.Request) {
	clearSessionCookie(w, r)
	http.Redirect(w, r, "/setup/login", http.StatusSeeOther)
}

func (a *app) passwordForm(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, http.StatusOK, "password", passwordView{page: a.page(r, "Change the password")})
}

// changePassword rotates the session key too, which signs every browser out,
// then signs this one back in.
func (a *app) changePassword(w http.ResponseWriter, r *http.Request) {
	snapshot := a.Store.Snapshot()
	view := passwordView{page: a.page(r, "Change the password")}
	switch password := r.PostFormValue("password"); {
	case !a.verifyPassword(r.Context(), snapshot.Admin.PasswordHash, r.PostFormValue("current")):
		view.Error = "the current password is wrong"
	case utf8.RuneCountInString(password) < MinPasswordLength:
		view.Error = fmt.Sprintf("the new password must be at least %d characters", MinPasswordLength)
	}
	if view.Error != "" {
		a.render(w, r, http.StatusBadRequest, "password", view)
		return
	}
	admin := &store.Admin{PasswordHash: HashPassword(r.PostFormValue("password")), SessionKey: NewSessionKey()}
	if err := a.Store.SetAdmin(admin); err != nil {
		a.fail(w, r, err)
		return
	}
	a.Log.Info("admin password changed", "from", clientAddress(r))
	a.setSessionCookie(w, r, admin)
	redirectNotice(w, r, "Password changed. Other browsers have been signed out.")
}

// ---- overview, public URL, secret

func (a *app) overview(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, http.StatusOK, "overview", a.overviewView(r, a.Store.Snapshot()))
}

func (a *app) setPublicURL(w http.ResponseWriter, r *http.Request) {
	value := strings.TrimRight(strings.TrimSpace(r.PostFormValue("public_url")), "/")
	if value != "" {
		if err := store.ValidatePublicURL(value); err != nil {
			view := a.overviewView(r, a.Store.Snapshot())
			view.Error = err.Error()
			view.PublicURL = value
			a.render(w, r, http.StatusBadRequest, "overview", view)
			return
		}
	}
	if err := a.Store.SetPublicURL(value); err != nil {
		a.fail(w, r, err)
		return
	}
	if value == "" {
		a.Log.Info("public url cleared")
		redirectNotice(w, r, "Public URL cleared. Server URLs cannot be shown until it is set again.")
		return
	}
	a.Log.Info("public url set", "public_url", value)
	redirectNotice(w, r, "Public URL saved.")
}

func (a *app) regenerateSecret(w http.ResponseWriter, r *http.Request) {
	if _, err := a.Store.RegenerateSecret(); err != nil {
		a.fail(w, r, err)
		return
	}
	a.Log.Warn("secret regenerated, every server URL has changed")
	redirectNotice(w, r, "Secret regenerated. Every server URL has changed; update your clients.")
}

// ---- server forms

func (a *app) newServerForm(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, http.StatusOK, "server", a.formView(r, "Add a server"))
}

func (a *app) editServerForm(w http.ResponseWriter, r *http.Request) {
	server, ok := a.Store.Snapshot().Server(r.PathValue("slug"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	view := a.formView(r, "Edit "+registry.DisplayName(kindName(server.Kind), server.Label))
	view.Editing, view.Slug, view.Kind, view.KindName = true, server.Slug, server.Kind, kindName(server.Kind)
	view.Label, view.URL, view.Library, view.LibraryName, view.Account, view.Auth, view.Enabled = server.Label, server.URL, server.Library, server.LibraryName, server.Account, server.Auth, server.Enabled
	view.Action = "/setup/servers/" + server.Slug
	a.render(w, r, http.StatusOK, "server", view)
}

// submitted is what the form posts, before any of it has been checked. The
// password is sent in the "password" field; it is left empty on an edit to
// keep the stored one.
type submitted struct {
	label    string
	url      string
	username string
	password string
	library  string
	enabled  bool
}

func readSubmitted(r *http.Request) submitted {
	return submitted{
		label:    strings.TrimSpace(r.PostFormValue("label")),
		url:      strings.TrimRight(strings.TrimSpace(r.PostFormValue("url")), "/"),
		username: strings.TrimSpace(r.PostFormValue("username")),
		password: r.PostFormValue("password"),
		library:  r.PostFormValue("library"),
		enabled:  r.PostFormValue("enabled") != "",
	}
}

func (a *app) createServer(w http.ResponseWriter, r *http.Request) {
	form := readSubmitted(r)
	view := a.formView(r, "Add a server")
	server, err := a.resolveServer(r.Context(), form, store.Server{Kind: store.Navidrome})
	if err != nil {
		a.renderServerError(w, r, view, form, err)
		return
	}
	added, err := a.Store.AddServer(server)
	if err != nil {
		a.renderServerError(w, r, view, form, err)
		return
	}
	a.apply()
	a.Log.Info("server added", "server", added.Slug, "source", string(added.Kind), "host", hostOf(added.URL), "username", added.Account)
	redirectNotice(w, r, registry.DisplayName(kindName(added.Kind), added.Label)+" added. Copy its URL into BitChord.")
}

func (a *app) updateServer(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	existing, ok := a.Store.Snapshot().Server(slug)
	if !ok {
		http.NotFound(w, r)
		return
	}
	form := readSubmitted(r)
	view := a.formView(r, "Edit "+registry.DisplayName(kindName(existing.Kind), existing.Label))
	view.Editing, view.Slug, view.Action = true, slug, "/setup/servers/"+slug
	if form.library == existing.Library {
		view.LibraryName = existing.LibraryName
	}
	server, err := a.resolveServer(r.Context(), form, existing)
	if err != nil {
		a.renderServerError(w, r, view, form, err)
		return
	}
	err = a.Store.UpdateServer(slug, func(current *store.Server) error {
		*current = server
		return nil
	})
	if err != nil {
		a.renderServerError(w, r, view, form, err)
		return
	}
	a.apply()
	a.Log.Info("server updated", "server", slug, "source", string(server.Kind), "host", hostOf(server.URL), "username", server.Account)
	redirectNotice(w, r, registry.DisplayName(kindName(server.Kind), server.Label)+" saved.")
}

// resolveServer turns a submitted form into a validated server: it settles the
// username and password, probes the server with them, and checks the library
// against what the server listed. An empty password on an edit keeps the
// stored one.
func (a *app) resolveServer(ctx context.Context, form submitted, existing store.Server) (store.Server, error) {
	server := existing
	server.Kind, server.Auth = store.Navidrome, store.AuthPassword
	server.Label, server.URL, server.Account, server.Enabled = form.label, form.url, form.username, form.enabled
	if err := store.ValidateServerURL(server.URL); err != nil {
		return server, err
	}
	if utf8.RuneCountInString(server.Label) > store.LabelMaxLength {
		return server, fmt.Errorf("the label must be at most %d characters", store.LabelMaxLength)
	}
	if server.Account == "" {
		return server, errors.New("a username is required")
	}
	switch {
	case form.password != "":
		server.Token = form.password
	case existing.Token != "":
		// Keep the stored password.
	default:
		return server, errors.New("a password is required")
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	probe, err := a.Registry.Probe(ctx, server)
	if err != nil {
		return server, describe(err)
	}
	server.Library, server.LibraryName = "", ""
	if form.library != "" {
		match := false
		for _, library := range probe.Libraries {
			if form.library == library.ID || form.library == library.Name {
				server.Library, server.LibraryName, match = library.ID, library.Name, true
			}
		}
		if !match {
			return server, errors.New("the server has no music library with that name")
		}
	}
	return server, nil
}

func (a *app) renderServerError(w http.ResponseWriter, r *http.Request, view serverFormView, form submitted, err error) {
	view.Error = err.Error()
	view.Kind, view.KindName = store.Navidrome, kindName(store.Navidrome)
	view.Label, view.URL, view.Account, view.Library, view.Enabled = form.label, form.url, form.username, form.library, form.enabled
	a.render(w, r, http.StatusBadRequest, "server", view)
}

func (a *app) deleteServer(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	err := a.Store.RemoveServer(slug)
	switch {
	case errors.Is(err, store.ErrNoServer):
		http.NotFound(w, r)
		return
	case err != nil:
		a.fail(w, r, err)
		return
	}
	a.apply()
	a.Log.Info("server removed", "server", slug)
	redirectNotice(w, r, slug+" removed. Its URL no longer answers.")
}

func (a *app) setServerEnabled(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	enabled := r.PostFormValue("enabled") == "true"
	err := a.Store.UpdateServer(slug, func(server *store.Server) error {
		server.Enabled = enabled
		return nil
	})
	switch {
	case errors.Is(err, store.ErrNoServer):
		http.NotFound(w, r)
		return
	case err != nil:
		a.fail(w, r, err)
		return
	}
	a.apply()
	a.Log.Info("server enabled changed", "server", slug, "enabled", enabled)
	redirectNotice(w, r, slug+" "+map[bool]string{true: "enabled", false: "disabled"}[enabled]+".")
}

// ---- JSON endpoints used by the form's buttons

type testRequest struct {
	URL      string `json:"url"`
	Username string `json:"username"`
	Password string `json:"password"`
	Slug     string `json:"slug"`
}

// testServer probes with the typed username and password, falling back to the
// stored password of the server being edited when the password is left empty,
// as a save would.
func (a *app) testServer(w http.ResponseWriter, r *http.Request) {
	var req testRequest
	if err := readJSON(r, &req); err != nil {
		jsonError(w, http.StatusBadRequest, "malformed request")
		return
	}
	req.URL = strings.TrimRight(strings.TrimSpace(req.URL), "/")
	if err := store.ValidateServerURL(req.URL); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	server := store.Server{Kind: store.Navidrome, URL: req.URL, Account: strings.TrimSpace(req.Username), Token: req.Password, Auth: store.AuthPassword}
	if req.Password == "" && req.Slug != "" {
		if existing, ok := a.Store.Snapshot().Server(req.Slug); ok {
			server.Token = existing.Token
			if server.Account == "" {
				server.Account = existing.Account
			}
		}
	}
	if server.Account == "" || server.Token == "" {
		jsonError(w, http.StatusBadRequest, "enter a username and password first")
		return
	}
	if !a.signins.attempt(req.URL) {
		jsonError(w, http.StatusTooManyRequests, "too many failed tests for this server, try again in a minute")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()
	probe, err := a.Registry.Probe(ctx, server)
	switch {
	case errors.Is(err, media.ErrUnauthorized):
		a.Log.Warn("navidrome test rejected", "host", hostOf(req.URL), "username", server.Account)
		jsonError(w, http.StatusUnauthorized, describe(err).Error())
		return
	case err != nil:
		a.signins.forgive(req.URL)
		jsonError(w, http.StatusBadGateway, describe(err).Error())
		return
	}
	a.signins.reset(req.URL)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": probe.Version, "libraries": libraryList(probe.Libraries)})
}

func libraryList(libraries []media.Library) []map[string]string {
	out := make([]map[string]string, len(libraries))
	for i, library := range libraries {
		out[i] = map[string]string{"id": library.ID, "name": library.Name}
	}
	return out
}

// describe turns an adapter error into a sentence for the page.
func describe(err error) error {
	switch {
	case errors.Is(err, media.ErrUnauthorized):
		return errors.New("the server rejected the credentials")
	case errors.Is(err, context.DeadlineExceeded):
		return errors.New("the server did not answer in time")
	}
	return err
}

func (a *app) fail(w http.ResponseWriter, r *http.Request, err error) {
	a.Log.Error("setup action failed", "path", r.URL.Path, "error", err.Error())
	view := a.overviewView(r, a.Store.Snapshot())
	view.Error = err.Error()
	a.render(w, r, http.StatusInternalServerError, "overview", view)
}
