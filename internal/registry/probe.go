package registry

import (
	"context"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/media"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/store"
)

type Probe struct {
	Version   string
	Libraries []media.Library
}

// Probe checks that a server answers with its credentials and lists its music
// libraries. Libraries is asked first because it needs a valid login, so a bad
// username or password is reported before the version is read.
func (r *Registry) Probe(ctx context.Context, server store.Server) (Probe, error) {
	backend, err := NewBackend(server, r.clientIDNow(), r.opts.Version, r.opts.Log)
	if err != nil {
		return Probe{}, err
	}
	libraries, err := backend.Libraries(ctx)
	if err != nil {
		return Probe{}, err
	}
	version, _ := backend.Version(ctx)
	return Probe{Version: version, Libraries: libraries}, nil
}

func (r *Registry) clientIDNow() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.clientID
}
