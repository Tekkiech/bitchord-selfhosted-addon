package registry

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/fakes"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/media"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/store"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func navidromeServer(fake *fakes.Navidrome, slug string) store.Server {
	return store.Server{Slug: slug, Kind: store.Navidrome, Label: "Home", URL: fake.URL, Account: fakes.NavidromeUser, Token: fakes.NavidromePassword, Auth: store.AuthPassword, Library: "Music", Enabled: true}
}

func snapshot(servers ...store.Server) store.Snapshot {
	return store.Snapshot{ClientID: "addon-uuid", Secret: "s3cr3t-s3cr3t-s3cr3t", Servers: servers}
}

func newRegistry(t *testing.T, snap store.Snapshot) *Registry {
	t.Helper()
	r := New(snap, Options{Interval: time.Hour, Version: "1.2.3", Log: quiet, ProbeTimeout: time.Second})
	t.Cleanup(r.Stop)
	return r
}

func waitHealthy(t *testing.T, r *Registry) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !r.Healthy() {
		if time.Now().After(deadline) {
			t.Fatal("never became healthy")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitReplaced(t *testing.T, r *Registry, slug string, before *Entry) *Entry {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if entry, ok := r.Lookup(slug); ok && entry != before {
			return entry
		}
		if time.Now().After(deadline) {
			t.Fatal("the entry was never replaced")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func stopped(t *testing.T, entry *Entry, what string) {
	t.Helper()
	select {
	case <-entry.done:
	case <-time.After(time.Second):
		t.Fatalf("%s still running", what)
	}
}

// holdSongs makes fake block every search3 until release is closed, and closes
// the returned channel when the first search arrives. It blocks the new
// entry's first refresh so a test can watch the handover.
func holdSongs(fake *fakes.Navidrome, release <-chan struct{}) <-chan struct{} {
	held := make(chan struct{})
	var once sync.Once
	fake.OnSearch = func(r *http.Request) {
		once.Do(func() { close(held) })
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}
	return held
}

func (r *Registry) pendingEntry(slug string) *Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.pending[slug].next
}

func TestAChangedServerAnswersFromTheOldIndexUntilTheNewOneLoads(t *testing.T) {
	old, next := fakes.NewNavidrome(t), fakes.NewNavidrome(t)
	next.Remove("103") // the new library has no "Closer".
	release := make(chan struct{})
	held := holdSongs(next, release)
	r := newRegistry(t, snapshot(navidromeServer(old, "navidrome-1")))
	waitHealthy(t, r)
	before, _ := r.Lookup("navidrome-1")

	moved := navidromeServer(next, "navidrome-1")
	r.Apply(snapshot(moved))
	<-held
	if entry, _ := r.Lookup("navidrome-1"); entry != before || len(entry.Library.Search("Closer", 5)) == 0 {
		t.Fatal("search stopped answering from the old index during the restart")
	}
	if !r.Healthy() {
		t.Fatal("unhealthy during the restart")
	}
	close(release)
	after := waitReplaced(t, r, "navidrome-1", before)
	if !after.Library.Ready() || len(after.Library.Search("Closer", 5)) != 0 || len(after.Library.Search("New Religion", 5)) == 0 {
		t.Fatal("the new entry does not answer from the new index")
	}
	stopped(t, before, "old refresher")
}

func TestAChangedServerSwapsWhenItsFirstRefreshFails(t *testing.T) {
	old, next := fakes.NewNavidrome(t), fakes.NewNavidrome(t)
	next.FailWith(http.StatusInternalServerError)
	r := newRegistry(t, snapshot(navidromeServer(old, "navidrome-1")))
	waitHealthy(t, r)
	before, _ := r.Lookup("navidrome-1")
	r.Apply(snapshot(navidromeServer(next, "navidrome-1")))
	waitReplaced(t, r, "navidrome-1", before)
	if status, _ := r.Status("navidrome-1"); status.Ready || !strings.Contains(status.LastError, "500") {
		t.Fatalf("status after a failed first refresh = %+v", status)
	}
	stopped(t, before, "old refresher")
}

func TestAChangedServerSwapsWhenItsFirstRefreshTakesTooLong(t *testing.T) {
	old, next := fakes.NewNavidrome(t), fakes.NewNavidrome(t)
	release := make(chan struct{})
	defer close(release)
	held := holdSongs(next, release)
	r := New(snapshot(navidromeServer(old, "navidrome-1")), Options{Interval: time.Hour, Log: quiet, HandoverTimeout: 100 * time.Millisecond})
	t.Cleanup(r.Stop)
	waitHealthy(t, r)
	before, _ := r.Lookup("navidrome-1")
	r.Apply(snapshot(navidromeServer(next, "navidrome-1")))
	<-held
	if after := waitReplaced(t, r, "navidrome-1", before); after.Library.Ready() {
		t.Fatal("swapped only after the refresh finished")
	}
	stopped(t, before, "old refresher")
}

func TestALaterChangeCancelsAPendingHandover(t *testing.T) {
	old, next := fakes.NewNavidrome(t), fakes.NewNavidrome(t)
	release := make(chan struct{})
	held := holdSongs(next, release)
	r := newRegistry(t, snapshot(navidromeServer(old, "navidrome-1")))
	waitHealthy(t, r)
	before, _ := r.Lookup("navidrome-1")

	r.Apply(snapshot(navidromeServer(next, "navidrome-1")))
	<-held
	pending := r.pendingEntry("navidrome-1")
	r.Apply(snapshot(navidromeServer(old, "navidrome-1")))
	stopped(t, pending, "cancelled refresher")
	close(release)
	time.Sleep(50 * time.Millisecond)
	if entry, _ := r.Lookup("navidrome-1"); entry != before {
		t.Fatal("a cancelled handover still swapped")
	}

	again := fakes.NewNavidrome(t)
	holdSongs(again, make(chan struct{}))
	r.Apply(snapshot(navidromeServer(again, "navidrome-1")))
	pending = r.pendingEntry("navidrome-1")
	r.Apply(snapshot())
	stopped(t, pending, "refresher of a removed server")
	stopped(t, before, "old refresher of a removed server")
	if _, ok := r.Lookup("navidrome-1"); ok {
		t.Fatal("removed server still listed")
	}
}

func TestAServerThatFailsToRestartKeepsItsOldEntry(t *testing.T) {
	fake := fakes.NewNavidrome(t)
	r := newRegistry(t, snapshot(navidromeServer(fake, "navidrome-1")))
	waitHealthy(t, r)
	before, _ := r.Lookup("navidrome-1")
	broken := navidromeServer(fake, "navidrome-1")
	broken.Kind, broken.Token = "emby", "next-password"
	r.Apply(snapshot(broken))
	if entry, ok := r.Lookup("navidrome-1"); !ok || entry != before {
		t.Fatal("a failed start must leave the old entry in place")
	}
}

func TestStopDuringAHandoverEndsBothRefreshers(t *testing.T) {
	old, next := fakes.NewNavidrome(t), fakes.NewNavidrome(t)
	held := holdSongs(next, make(chan struct{}))
	r := New(snapshot(navidromeServer(old, "navidrome-1")), Options{Interval: time.Hour, Log: quiet})
	waitHealthy(t, r)
	before, _ := r.Lookup("navidrome-1")
	r.Apply(snapshot(navidromeServer(next, "navidrome-1")))
	<-held
	pending := r.pendingEntry("navidrome-1")
	r.Stop()
	stopped(t, before, "old refresher")
	stopped(t, pending, "pending refresher")
	if _, ok := r.Lookup("navidrome-1"); ok {
		t.Fatal("entry still listed after Stop")
	}
}

func TestNewStartsOneRefresherPerEnabledServer(t *testing.T) {
	first, second := fakes.NewNavidrome(t), fakes.NewNavidrome(t)
	disabled := navidromeServer(first, "navidrome-2")
	disabled.Enabled = false
	r := newRegistry(t, snapshot(navidromeServer(first, "navidrome-1"), navidromeServer(second, "navidrome-3"), disabled))
	waitHealthy(t, r)
	entry, ok := r.Lookup("navidrome-1")
	if !ok || entry.DisplayName() != "Navidrome - Home" || entry.Kind != store.Navidrome {
		t.Fatalf("navidrome-1 = %+v, %v", entry, ok)
	}
	if _, ok := r.Lookup("navidrome-2"); ok {
		t.Error("a disabled server was started")
	}
	status, ok := r.Status("navidrome-1")
	if !ok || !status.Ready || status.Tracks != 6 || status.Refreshed.IsZero() || status.LastError != "" {
		t.Fatalf("status = %+v", status)
	}
	if _, ok := r.Status("nope"); ok {
		t.Error("status for an unknown slug")
	}
	if _, found := entry.Library.Get("101"); !found {
		t.Error("index empty")
	}
}

func TestApplyStartsStopsAndReplacesEntries(t *testing.T) {
	fake := fakes.NewNavidrome(t)
	r := newRegistry(t, snapshot(navidromeServer(fake, "navidrome-1")))
	waitHealthy(t, r)
	before, _ := r.Lookup("navidrome-1")

	relabelled := navidromeServer(fake, "navidrome-1")
	relabelled.Label = "Parents"
	r.Apply(snapshot(relabelled))
	after, _ := r.Lookup("navidrome-1")
	if after != before || after.DisplayName() != "Navidrome - Parents" {
		t.Fatal("a label change must not restart the entry")
	}

	other := fakes.NewNavidrome(t)
	moved := navidromeServer(other, "navidrome-1")
	r.Apply(snapshot(moved))
	waitReplaced(t, r, "navidrome-1", before)
	stopped(t, before, "old refresher")
	waitHealthy(t, r)
	if requests := other.Requests(); len(requests) == 0 {
		t.Fatal("new entry never asked the new server")
	}

	added := navidromeServer(fake, "navidrome-2")
	r.Apply(snapshot(moved, added))
	if _, ok := r.Lookup("navidrome-2"); !ok {
		t.Fatal("added server not started")
	}

	disabled := moved
	disabled.Enabled = false
	r.Apply(snapshot(disabled, added))
	if _, ok := r.Lookup("navidrome-1"); ok {
		t.Fatal("disabled server still running")
	}
	r.Apply(snapshot())
	if _, ok := r.Lookup("navidrome-2"); ok {
		t.Fatal("removed server still running")
	}
	if !r.Healthy() {
		t.Fatal("empty registry must be healthy")
	}
}

func TestHealthyWaitsForEveryFirstLoad(t *testing.T) {
	good, bad := fakes.NewNavidrome(t), fakes.NewNavidrome(t)
	bad.FailWith(http.StatusInternalServerError)
	r := newRegistry(t, snapshot(navidromeServer(good, "navidrome-1"), navidromeServer(bad, "navidrome-2")))
	deadline := time.Now().Add(2 * time.Second)
	for {
		status, _ := r.Status("navidrome-2")
		if status.LastError != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("failing server never reported an error")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if r.Healthy() {
		t.Fatal("healthy while one server has never loaded")
	}
	if status, _ := r.Status("navidrome-2"); status.Ready || !strings.Contains(status.LastError, "500") {
		t.Fatalf("failing status = %+v", status)
	}
	bad.FailWith(0)
	r.Apply(snapshot(navidromeServer(good, "navidrome-1")))
	waitHealthy(t, r)
}

func TestStopEndsEveryRefresher(t *testing.T) {
	fake := fakes.NewNavidrome(t)
	blocked := make(chan struct{})
	var once sync.Once
	fake.OnSearch = func(r *http.Request) {
		once.Do(func() { close(blocked) })
		<-r.Context().Done()
	}
	r := New(snapshot(navidromeServer(fake, "navidrome-1")), Options{Interval: time.Hour, Log: quiet})
	entry, _ := r.Lookup("navidrome-1")
	<-blocked
	r.Stop()
	select {
	case <-entry.done:
	case <-time.After(time.Second):
		t.Fatal("refresher outlived Stop")
	}
	if _, ok := r.Lookup("navidrome-1"); ok {
		t.Fatal("entry still listed after Stop")
	}
}

func TestProbeListsLibrariesAndRejectsBadCredentials(t *testing.T) {
	r := newRegistry(t, snapshot())
	fake := fakes.NewNavidrome(t)
	got, err := r.Probe(context.Background(), store.Server{Kind: store.Navidrome, URL: fake.URL, Account: fakes.NavidromeUser, Token: fakes.NavidromePassword})
	if err != nil || got.Version != "0.64.2" || len(got.Libraries) != 2 || got.Libraries[0] != (media.Library{ID: "1", Name: "Music"}) {
		t.Fatalf("probe = %+v, %v", got, err)
	}
	if _, err := r.Probe(context.Background(), store.Server{Kind: store.Navidrome, URL: fake.URL, Account: fakes.NavidromeUser, Token: "wrong"}); !errors.Is(err, media.ErrUnauthorized) {
		t.Fatalf("wrong password: %v", err)
	}
	if _, err := r.Probe(context.Background(), store.Server{Kind: "emby", URL: fake.URL, Token: "x"}); err == nil {
		t.Fatal("unknown kind accepted")
	}
}

func TestDisplayNameIsSafeUnderConcurrentRelabel(t *testing.T) {
	fake := fakes.NewNavidrome(t)
	r := newRegistry(t, snapshot(navidromeServer(fake, "navidrome-1")))
	waitHealthy(t, r)
	entry, _ := r.Lookup("navidrome-1")

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				entry.DisplayName()
			}
		}
	}()

	for i := 0; i < 200; i++ {
		relabelled := navidromeServer(fake, "navidrome-1")
		if i%2 == 0 {
			relabelled.Label = "Parents"
		} else {
			relabelled.Label = "Home"
		}
		r.Apply(snapshot(relabelled))
	}
	close(stop)
	<-done
}
