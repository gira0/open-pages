package storage_test

import (
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/gira0/open-pages/internal/storage"
	"github.com/gira0/open-pages/internal/storage/storagetest"
	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

// TestS3 runs the conformance suite against an in-process fake S3. Set
// OPEN_PAGES_TEST_S3_ENDPOINT, _BUCKET, _ACCESS_KEY and _SECRET_KEY to run it against a
// real service instead (CI uses Garage); each case then gets its own key prefix.
func TestS3(t *testing.T) {
	cfg := storage.S3Config{Region: "us-east-1", PathStyle: true, AccessKey: "test", SecretKey: "test-secret"}
	if ep := os.Getenv("OPEN_PAGES_TEST_S3_ENDPOINT"); ep != "" {
		cfg.Endpoint = ep
		cfg.Bucket = os.Getenv("OPEN_PAGES_TEST_S3_BUCKET")
		cfg.AccessKey = os.Getenv("OPEN_PAGES_TEST_S3_ACCESS_KEY")
		cfg.SecretKey = os.Getenv("OPEN_PAGES_TEST_S3_SECRET_KEY")
		ensureBucket(t, cfg)
	} else {
		backend := s3mem.New()
		if err := backend.CreateBucket("pages"); err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(gofakes3.New(backend).Server())
		t.Cleanup(srv.Close)
		cfg.Endpoint = srv.URL
		cfg.Bucket = "pages"
	}
	n := 0
	storagetest.Run(t, func(t *testing.T) storage.Store {
		n++
		c := cfg
		c.Prefix = fmt.Sprintf("t%d-%d", time.Now().UnixNano(), n)
		c.StagingDir = t.TempDir()
		s, err := storage.NewS3(t.Context(), c)
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}

// ensureBucket creates the bucket on a real service if it is not there yet.
func ensureBucket(t *testing.T, cfg storage.S3Config) {
	t.Helper()
	c := s3.New(s3.Options{
		Region: cfg.Region, BaseEndpoint: aws.String(cfg.Endpoint), UsePathStyle: true,
		Credentials:                credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
	})
	_, err := c.CreateBucket(t.Context(), &s3.CreateBucketInput{Bucket: &cfg.Bucket})
	var api smithy.APIError
	if err == nil {
		return
	}
	if errors.As(err, &api) && (api.ErrorCode() == "BucketAlreadyOwnedByYou" || api.ErrorCode() == "BucketAlreadyExists") {
		return
	}
	t.Fatalf("create bucket: %v", err)
}
