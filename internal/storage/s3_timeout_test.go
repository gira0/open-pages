package storage

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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
