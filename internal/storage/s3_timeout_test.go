package storage

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

// A store that accepts the connection and never answers must fail the call at its deadline
// instead of holding the caller (and with it the server's deploy lock) for ever.
func TestS3CallsTimeOutOnHungServer(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	t.Cleanup(func() { close(release); srv.Close() })

	old := metaTimeout
	metaTimeout = 200 * time.Millisecond
	t.Cleanup(func() { metaTimeout = old })

	s, err := NewS3(t.Context(), S3Config{
		Endpoint: srv.URL, Region: "us-east-1", Bucket: "b", PathStyle: true,
		AccessKey: "a", SecretKey: "s", StagingDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := s.Current(t.Context(), "blog"); err == nil {
		t.Fatal("Current succeeded against a hung server")
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("Current took %v, want it to stop near the 200ms deadline", took)
	}
}

// A store that sends part of an object and then goes quiet must fail the visitor's read
// after the stall timeout, not hold it for ever; a store that keeps sending slowly must not.
func TestS3ReadStallTimesOut(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", "1000")
			return
		}
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		<-release
	}))
	t.Cleanup(func() { close(release); srv.Close() })

	old := readStallTimeout
	readStallTimeout = 200 * time.Millisecond
	t.Cleanup(func() { readStallTimeout = old })

	s, err := NewS3(t.Context(), S3Config{
		Endpoint: srv.URL, Region: "us-east-1", Bucket: "b", PathStyle: true,
		AccessKey: "a", SecretKey: "s", StagingDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	root := &s3Root{s: s, ctx: t.Context(), prefix: "sites/blog/v1/"}
	f, err := root.Open("index.html")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	start := time.Now()
	_, err = io.ReadAll(f)
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("ReadAll = %v, want a deadline error", err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("read took %v, want it to stop near the 200ms stall timeout", took)
	}
}

// A body that arrives in steady pieces takes longer than the stall timeout in total and
// still succeeds: the timeout counts waiting on the store, not the whole transfer.
func TestS3SlowSteadyReadSucceeds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "10")
		if r.Method == http.MethodHead {
			return
		}
		w.WriteHeader(http.StatusPartialContent)
		for range 10 {
			_, _ = w.Write([]byte("x"))
			w.(http.Flusher).Flush()
			time.Sleep(60 * time.Millisecond)
		}
	}))
	t.Cleanup(srv.Close)

	old := readStallTimeout
	readStallTimeout = 300 * time.Millisecond
	t.Cleanup(func() { readStallTimeout = old })

	s, err := NewS3(t.Context(), S3Config{
		Endpoint: srv.URL, Region: "us-east-1", Bucket: "b", PathStyle: true,
		AccessKey: "a", SecretKey: "s", StagingDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	root := &s3Root{s: s, ctx: t.Context(), prefix: "sites/blog/v1/"}
	f, err := root.Open("index.html")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil || len(b) != 10 {
		t.Fatalf("ReadAll = %d bytes, %v", len(b), err)
	}
}

// A body that keeps arriving, so the stall timeout never fires, still ends at the total
// timeout: a store that trickles for ever cannot hold the request.
func TestS3ReadTotalTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		if r.Method == http.MethodHead {
			return
		}
		w.WriteHeader(http.StatusPartialContent)
		for range 1000 {
			if _, err := w.Write([]byte("x")); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-time.After(50 * time.Millisecond):
			case <-release:
				return
			}
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })

	oldStall, oldTotal := readStallTimeout, readTotalTimeout
	readStallTimeout, readTotalTimeout = 5*time.Second, 400*time.Millisecond
	t.Cleanup(func() { readStallTimeout, readTotalTimeout = oldStall, oldTotal })

	s, err := NewS3(t.Context(), S3Config{
		Endpoint: srv.URL, Region: "us-east-1", Bucket: "b", PathStyle: true,
		AccessKey: "a", SecretKey: "s", StagingDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	root := &s3Root{s: s, ctx: t.Context(), prefix: "sites/blog/v1/"}
	f, err := root.Open("index.html")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	start := time.Now()
	_, err = io.ReadAll(f)
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("ReadAll = %v, want a deadline error", err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("read took %v, want it to stop near the 400ms total timeout", took)
	}
}

// Deleting a version removes the marker and then the files; a cancelled caller must not
// stop between the two and strand files that no marker names.
func TestS3DeleteVersionSurvivesCancelledContext(t *testing.T) {
	backend := s3mem.New()
	if err := backend.CreateBucket("pages"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(gofakes3.New(backend).Server())
	t.Cleanup(srv.Close)
	s, err := NewS3(t.Context(), S3Config{
		Endpoint: srv.URL, Region: "us-east-1", Bucket: "pages", PathStyle: true,
		AccessKey: "a", SecretKey: "s", StagingDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for _, n := range []string{"index.html", "a.css"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	const v = "d0000000000000000000"
	if err := s.Publish(t.Context(), "blog", v, dir); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.DeleteVersion(ctx, "blog", v); err != nil {
		t.Fatalf("DeleteVersion with a cancelled context = %v", err)
	}
	var keys []string
	if err := s.list(t.Context(), s.siteKey("blog"), func(k string) error { keys = append(keys, k); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 {
		t.Fatalf("objects left after DeleteVersion: %v", keys)
	}
}
