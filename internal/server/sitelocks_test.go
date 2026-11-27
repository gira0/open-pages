package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gira0/open-pages/internal/storage"
)

// blockPublishStore blocks Publish of one site until released.
type blockPublishStore struct {
	storage.Store
	site    string
	started chan struct{}
	release chan struct{}
}

func (st blockPublishStore) Publish(ctx context.Context, site, version, staging string) error {
	if site == st.site {
		close(st.started)
		<-st.release
	}
	return st.Store.Publish(ctx, site, version, staging)
}

// A slow publish (an S3 upload) of one site must not stall deploys, version listings or
// deletes of other sites.
func TestSlowPublishDoesNotBlockOtherSites(t *testing.T) {
	_, s := newTestServer(t)
	owner := testOwner(t, s)
	for _, name := range []string{"slow", "fast"} {
		if _, err := s.createSite(t.Context(), name, "", owner, 0, visPublic); err != nil {
			t.Fatal(err)
		}
	}
	deployNamedFiles(t, s, "fast")
	st := blockPublishStore{Store: s.store, site: "slow", started: make(chan struct{}), release: make(chan struct{})}
	s.store = st

	slowDone := make(chan error, 1)
	go func() {
		_, err := s.deploy(t.Context(), mustSite(t, s, "slow"), writeZip(t, map[string]string{"index.html": "x"}))
		slowDone <- err
	}()
	<-st.started

	others := make(chan error, 1)
	go func() {
		if _, err := s.deploy(t.Context(), mustSite(t, s, "fast"), writeZip(t, map[string]string{"index.html": "y"})); err != nil {
			others <- err
			return
		}
		if _, _, err := s.versionsAndCurrent(t.Context(), "fast"); err != nil {
			others <- err
			return
		}
		others <- s.deleteSite(t.Context(), mustSite(t, s, "fast"))
	}()
	select {
	case err := <-others:
		if err != nil {
			t.Fatalf("operations on another site: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("another site's operations are blocked by a slow publish")
	}
	close(st.release)
	if err := <-slowDone; err != nil {
		t.Fatal(err)
	}
}

// The user quota still holds when two sites of one owner deploy at the same time.
func TestConcurrentDeploysShareUserQuota(t *testing.T) {
	_, s := newTestServer(t)
	s.cfg.UserQuota = 1000
	owner := testOwner(t, s)
	for _, name := range []string{"slow", "fast"} {
		if _, err := s.createSite(t.Context(), name, "", owner, 0, visPublic); err != nil {
			t.Fatal(err)
		}
	}
	st := blockPublishStore{Store: s.store, site: "slow", started: make(chan struct{}), release: make(chan struct{})}
	s.store = st
	slowDone := make(chan error, 1)
	go func() {
		_, err := s.deploy(t.Context(), mustSite(t, s, "slow"), writeZip(t, payload(600)))
		slowDone <- err
	}()
	<-st.started // 600 bytes reserved, not yet recorded

	var qe *quotaError
	if _, err := s.deploy(t.Context(), mustSite(t, s, "fast"), writeZip(t, payload(600))); !errors.As(err, &qe) {
		t.Fatalf("second deploy while the first is in flight = %v, want a quota error", err)
	}
	close(st.release)
	if err := <-slowDone; err != nil {
		t.Fatal(err)
	}
}
