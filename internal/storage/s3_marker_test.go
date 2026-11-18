package storage

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

// markerStore returns an S3 over a fake whose marker PUTs reach the backend but whose
// response is lost (the connection is cut). With failReadBack, reading a marker also fails,
// so Publish cannot tell whether the PUT landed. It also returns the PUT count.
func markerStore(t *testing.T, failReadBack bool) (*S3, *atomic.Int32) {
	t.Helper()
	backend := s3mem.New()
	if err := backend.CreateBucket("pages"); err != nil {
		t.Fatal(err)
	}
	fake := gofakes3.New(backend).Server()
	var puts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isMarker := strings.Contains(r.URL.Path, "/_versions/")
		switch {
		case isMarker && r.Method == http.MethodPut:
			puts.Add(1)
			fake.ServeHTTP(httptest.NewRecorder(), r) // lands in the store; the response is dropped
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
		case isMarker && r.Method == http.MethodGet && failReadBack:
			http.Error(w, "denied", http.StatusForbidden)
		default:
			fake.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	s, err := NewS3(t.Context(), S3Config{
		Endpoint: srv.URL, Region: "us-east-1", Bucket: "pages", PathStyle: true,
		AccessKey: "a", SecretKey: "s", StagingDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, &puts
}

func stagedSite(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A marker PUT whose response is lost may have succeeded. Publish reads the marker back and,
// finding it whole, reports success instead of failing the deploy and deleting the files
// the marker names.
func TestS3PublishSurvivesLostMarkerResponse(t *testing.T) {
	s, puts := markerStore(t, false)
	const v = "d0000000000000000000"
	if err := s.Publish(t.Context(), "blog", v, stagedSite(t)); err != nil {
		t.Fatalf("Publish = %v, want success: the marker landed", err)
	}
	if puts.Load() == 0 {
		t.Fatal("marker was never PUT")
	}
	if vs, err := s.Versions(t.Context(), "blog"); err != nil || len(vs) != 1 || vs[0] != v {
		t.Fatalf("Versions = %v, %v", vs, err)
	}
	if n, err := s.VersionSize(t.Context(), "blog", v); err != nil || n != 5 {
		t.Fatalf("VersionSize = %d, %v", n, err)
	}
}

// If the marker cannot be read back either, the publish fails, and the marker the lost PUT
// may have left behind is removed along with the files, so no version without files shows.
func TestS3PublishLostMarkerResponseLeavesNothing(t *testing.T) {
	s, _ := markerStore(t, true)
	const v = "d0000000000000000000"
	if err := s.Publish(t.Context(), "blog", v, stagedSite(t)); err == nil {
		t.Fatal("Publish succeeded though the marker could not be confirmed")
	}
	if vs, err := s.Versions(t.Context(), "blog"); err != nil || len(vs) != 0 {
		t.Fatalf("Versions = %v, %v; want none", vs, err)
	}
	var keys []string
	if err := s.list(t.Context(), s.siteKey("blog"), func(k string) error { keys = append(keys, k); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 {
		t.Fatalf("stray objects left: %v", keys)
	}
}
