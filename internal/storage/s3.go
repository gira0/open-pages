package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/gira0/open-pages/internal/names"
)

// S3Config configures an S3 store. It uses only the plain S3 object API (PUT, GET with
// ranges, HEAD, LIST, multi-object DELETE), so any compatible service works.
type S3Config struct {
	Endpoint   string // base URL; empty means Amazon S3
	Region     string
	Bucket     string // must exist
	Prefix     string // key prefix without leading or trailing slash; may be empty
	PathStyle  bool
	AccessKey  string // both empty means the SDK default credential chain
	SecretKey  string
	StagingDir string // local directory for extraction; must exist and be writable
}

// uploadWorkers is how many objects Publish uploads at once.
const uploadWorkers = 8

// Deadlines for calls the server makes under its deploy lock. They are variables so a test
// can shorten them.
var (

	// The server holds its deploy lock while it calls the store, and the AWS SDK sets no
	// deadline of its own, so every call that can run under that lock has one. A hung
	// connection then fails the deploy instead of freezing every deploy, rollback and delete.
	metaTimeout    = 30 * time.Second // one small request: HEAD, GET or PUT of a marker
	bulkTimeout    = 5 * time.Minute  // deleting a version or a site: a LIST and batched DELETEs
	publishTimeout = 15 * time.Minute // uploading every file of a version
	// responseHeaderTimeout bounds the wait for the first byte of any response, including
	// the reads that serve a file, which run under the visitor's request context only.
	responseHeaderTimeout = 60 * time.Second
	// readStallTimeout is how long a visitor's read may wait on the store for the next
	// bytes. The whole body gets no deadline, because a slow visitor legitimately takes
	// long and the server sets no WriteTimeout for the same reason; only the time spent
	// waiting on the store counts, so a store that goes quiet mid-body cannot hold the
	// request, and its connection, for ever.
	readStallTimeout = 60 * time.Second
)

// S3 keeps sites in an object store:
//
//	<prefix>/sites/<site>/<version>/<file>      the files of a version
//	<prefix>/sites/<site>/_versions/<version>   marker, written last: the version exists
//	<prefix>/sites/<site>/.current              the live version id
//
// The marker directory is not called ".versions" because SeaweedFS reserves that name for its
// own object versioning and hides what is stored under it (found by the CI conformance job).
//
// A version appears in Versions only once its marker exists, so an interrupted publish
// leaves unreachable objects but never a half-visible version. The marker's body is the
// version's size in bytes. SetCurrent is one PUT of .current, which S3 makes visible to
// every later read at once, so readers see the old or the new version, never a mix.
type S3 struct {
	c       *s3.Client
	bucket  string
	prefix  string
	staging string
}

// NewS3 connects to the object store described by cfg. It makes no request; Init does.
func NewS3(ctx context.Context, cfg S3Config) (*S3, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("s3: bucket is required")
	}
	httpClient := awshttp.NewBuildableClient().WithTransportOptions(func(tr *http.Transport) {
		tr.ResponseHeaderTimeout = responseHeaderTimeout
	})
	var base aws.Config
	if cfg.AccessKey != "" {
		base = aws.Config{Credentials: credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, "")}
	} else {
		var err error
		if base, err = awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.Region), awsconfig.WithHTTPClient(httpClient)); err != nil {
			return nil, fmt.Errorf("s3: load credentials: %w", err)
		}
	}
	base.Region = cfg.Region
	base.HTTPClient = httpClient
	client := s3.NewFromConfig(base, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		o.UsePathStyle = cfg.PathStyle
		// Do not add checksum headers the service did not ask for: several compatible
		// servers reject them.
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	})
	prefix := strings.Trim(cfg.Prefix, "/")
	if prefix != "" {
		prefix += "/"
	}
	return &S3{c: client, bucket: cfg.Bucket, prefix: prefix, staging: cfg.StagingDir}, nil
}

func (s *S3) siteKey(site string) string             { return s.prefix + "sites/" + site + "/" }
func (s *S3) versionKey(site, version string) string { return s.siteKey(site) + version + "/" }
func (s *S3) markerKey(site, version string) string  { return s.siteKey(site) + "_versions/" + version }
func (s *S3) currentKey(site string) string          { return s.siteKey(site) + ".current" }
func (s *S3) healthKey() string                      { return s.prefix + ".healthz" }
func validS3Site(site string) bool                   { return names.ValidSite(site) }
func isNotFound(err error) bool {
	var api smithy.APIError
	if errors.As(err, &api) {
		switch api.ErrorCode() {
		case "NotFound", "NoSuchKey":
			return true
		}
	}
	var nf *s3types.NotFound
	var nk *s3types.NoSuchKey
	return errors.As(err, &nf) || errors.As(err, &nk)
}

// Init checks that the bucket exists and the staging directory is usable.
func (s *S3) Init(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, metaTimeout)
	defer cancel()
	if _, err := s.c.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &s.bucket}); err != nil {
		return fmt.Errorf("s3 bucket %q: %w", s.bucket, err)
	}
	if err := os.MkdirAll(s.staging, 0o770); err != nil { //nolint:gosec // G301: group 0 shares the volume under OpenShift arbitrary UIDs
		return fmt.Errorf("create %s: %w", s.staging, err)
	}
	return nil
}

// Check proves the bucket accepts writes by putting and deleting a small object.
func (s *S3) Check(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, metaTimeout)
	defer cancel()
	key := s.healthKey()
	if _, err := s.c.PutObject(ctx, &s3.PutObjectInput{Bucket: &s.bucket, Key: &key, Body: bytes.NewReader([]byte("ok"))}); err != nil {
		return err
	}
	_, err := s.c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &s.bucket, Key: &key})
	return err
}

// Staging returns an empty directory under the staging directory.
func (s *S3) Staging(context.Context) (string, func(), error) {
	dir, err := os.MkdirTemp(s.staging, ".staging-*")
	if err != nil {
		return "", nil, err
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

// Publish uploads every regular file of the staged directory, then writes the marker.
// Symbolic links and other special files are not uploaded.
func (s *S3) Publish(ctx context.Context, site, version, stagingDir string) error {
	ctx, cancelTimeout := context.WithTimeout(ctx, publishTimeout)
	defer cancelTimeout()
	if !validS3Site(site) || !VersionRe.MatchString(version) {
		return ErrInvalid
	}
	type file struct {
		path string
		key  string
		size int64
	}
	var files []file
	var total int64
	prefix := s.versionKey(site, version)
	err := filepath.WalkDir(stagingDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(stagingDir, p)
		if err != nil {
			return err
		}
		files = append(files, file{p, prefix + filepath.ToSlash(rel), info.Size()})
		total += info.Size()
		return nil
	})
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
		jobs     = make(chan file)
	)
	fail := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		if firstErr == nil {
			firstErr = err
			cancel()
		}
	}
	for range min(uploadWorkers, max(len(files), 1)) {
		wg.Go(func() {
			for f := range jobs {
				if err := s.upload(ctx, f.path, f.key, f.size); err != nil {
					fail(fmt.Errorf("upload %s: %w", strings.TrimPrefix(f.key, prefix), err))
				}
			}
		})
	}
feed:
	for _, f := range files {
		select {
		case jobs <- f:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	if firstErr == nil {
		firstErr = ctx.Err()
	}
	if firstErr != nil {
		// Leave nothing unreachable behind; the caller's context may be the reason we failed.
		s.cleanup(ctx, prefix)
		return firstErr
	}
	marker := s.markerKey(site, version)
	_, err = s.c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: &s.bucket, Key: &marker, Body: strings.NewReader(strconv.FormatInt(total, 10)),
	})
	if err != nil {
		s.cleanup(ctx, prefix)
		return fmt.Errorf("write version marker: %w", err)
	}
	return nil
}

// cleanup removes the objects of a failed publish. The caller's context may be the reason
// the publish failed, so it is detached from it, but the cleanup still gets a deadline: the
// server holds its deploy lock meanwhile.
func (s *S3) cleanup(ctx context.Context, prefix string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bulkTimeout)
	defer cancel()
	_ = s.deletePrefix(ctx, prefix)
}

func (s *S3) upload(ctx context.Context, local, key string, size int64) error {
	f, err := os.Open(local) //nolint:gosec // G304: local is a file inside the staging directory we walked
	if err != nil {
		return err
	}
	defer f.Close()
	in := &s3.PutObjectInput{Bucket: &s.bucket, Key: &key, Body: f, ContentLength: &size}
	if ct := mime.TypeByExtension(path.Ext(key)); ct != "" {
		in.ContentType = &ct
	}
	_, err = s.c.PutObject(ctx, in)
	return err
}

// list calls fn for every key under prefix.
func (s *S3) list(ctx context.Context, prefix string, fn func(key string) error) error {
	p := s3.NewListObjectsV2Paginator(s.c, &s3.ListObjectsV2Input{Bucket: &s.bucket, Prefix: &prefix})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, o := range page.Contents {
			if err := fn(aws.ToString(o.Key)); err != nil {
				return err
			}
		}
	}
	return nil
}

// deletePrefix removes every object under prefix, in batches of up to 1000 keys.
func (s *S3) deletePrefix(ctx context.Context, prefix string) error {
	var batch []s3types.ObjectIdentifier
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		out, err := s.c.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: &s.bucket, Delete: &s3types.Delete{Objects: batch, Quiet: aws.Bool(true)},
		})
		batch = batch[:0]
		if err != nil {
			return err
		}
		if len(out.Errors) > 0 {
			e := out.Errors[0]
			return fmt.Errorf("delete %s: %s: %s", aws.ToString(e.Key), aws.ToString(e.Code), aws.ToString(e.Message))
		}
		return nil
	}
	err := s.list(ctx, prefix, func(key string) error {
		batch = append(batch, s3types.ObjectIdentifier{Key: aws.String(key)})
		if len(batch) == 1000 {
			return flush()
		}
		return nil
	})
	if err != nil {
		return err
	}
	return flush()
}

// Versions lists the markers of site, oldest first.
func (s *S3) Versions(ctx context.Context, site string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, bulkTimeout)
	defer cancel()
	if !validS3Site(site) {
		return nil, ErrInvalid
	}
	prefix := s.siteKey(site) + "_versions/"
	var out []string
	err := s.list(ctx, prefix, func(key string) error {
		if v := strings.TrimPrefix(key, prefix); VersionRe.MatchString(v) {
			out = append(out, v)
		}
		return nil
	})
	slices.Sort(out)
	return out, err
}

// Current reads the .current object.
func (s *S3) Current(ctx context.Context, site string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, metaTimeout)
	defer cancel()
	if !validS3Site(site) {
		return "", ErrInvalid
	}
	key := s.currentKey(site)
	out, err := s.c.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &key})
	if isNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer out.Body.Close()
	b, err := io.ReadAll(io.LimitReader(out.Body, 64))
	if err != nil {
		return "", err
	}
	v := strings.TrimSpace(string(b))
	if !VersionRe.MatchString(v) {
		return "", fmt.Errorf("s3: %s holds %q, not a version", key, v)
	}
	return v, nil
}

// SetCurrent writes the .current object after checking the version's marker.
func (s *S3) SetCurrent(ctx context.Context, site, version string) error {
	ctx, cancel := context.WithTimeout(ctx, metaTimeout)
	defer cancel()
	if !validS3Site(site) {
		return ErrInvalid
	}
	if !VersionRe.MatchString(version) {
		return ErrVersionAbsent
	}
	if _, err := s.VersionSize(ctx, site, version); err != nil {
		return err
	}
	key := s.currentKey(site)
	_, err := s.c.PutObject(ctx, &s3.PutObjectInput{Bucket: &s.bucket, Key: &key, Body: strings.NewReader(version)})
	return err
}

// VersionSize reads the size recorded in the version's marker.
func (s *S3) VersionSize(ctx context.Context, site, version string) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, metaTimeout)
	defer cancel()
	if !validS3Site(site) || !VersionRe.MatchString(version) {
		return 0, ErrInvalid
	}
	key := s.markerKey(site, version)
	out, err := s.c.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &key})
	if isNotFound(err) {
		return 0, ErrVersionAbsent
	}
	if err != nil {
		return 0, err
	}
	defer out.Body.Close()
	b, err := io.ReadAll(io.LimitReader(out.Body, 32))
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("s3: %s holds %q, not a size", key, b)
	}
	return n, nil
}

// DeleteVersion removes the marker first, so the version disappears from Versions before
// its objects do, then the objects.
func (s *S3) DeleteVersion(ctx context.Context, site, version string) error {
	ctx, cancel := context.WithTimeout(ctx, bulkTimeout)
	defer cancel()
	if !validS3Site(site) || !VersionRe.MatchString(version) {
		return ErrInvalid
	}
	marker := s.markerKey(site, version)
	if _, err := s.c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &s.bucket, Key: &marker}); err != nil && !isNotFound(err) {
		return err
	}
	return s.deletePrefix(ctx, s.versionKey(site, version))
}

// DeleteSite removes .current first, so the site stops being served, then everything.
func (s *S3) DeleteSite(ctx context.Context, site string) error {
	ctx, cancel := context.WithTimeout(ctx, bulkTimeout)
	defer cancel()
	if !validS3Site(site) {
		return ErrInvalid
	}
	key := s.currentKey(site)
	if _, err := s.c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &s.bucket, Key: &key}); err != nil && !isNotFound(err) {
		return err
	}
	return s.deletePrefix(ctx, s.siteKey(site))
}

// OpenCurrent opens the live version. The Root uses ctx for every request it makes, so it
// must not outlive the request that opened it.
func (s *S3) OpenCurrent(ctx context.Context, site string) (Root, string, error) {
	version, err := s.Current(ctx, site)
	if err != nil {
		return nil, "", err
	}
	if version == "" {
		return nil, "", ErrNotDeployed
	}
	return &s3Root{s: s, ctx: ctx, prefix: s.versionKey(site, version)}, version, nil
}

type s3Root struct {
	s      *S3
	ctx    context.Context
	prefix string
}

func (r *s3Root) Close() error { return nil }

// Open returns the object at name, or a directory if objects exist below name.
func (r *s3Root) Open(name string) (File, error) {
	notExist := &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	if name == "" || strings.ContainsRune(name, 0) || path.IsAbs(name) || name != path.Clean(name) ||
		name == ".." || strings.HasPrefix(name, "../") {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	if name == "." {
		return &s3Dir{name: "."}, nil
	}
	key := r.prefix + name
	ctx, cancel := context.WithTimeout(r.ctx, metaTimeout)
	defer cancel()
	head, err := r.s.c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &r.s.bucket, Key: &key})
	if err == nil {
		return &s3File{r: r, key: key, name: path.Base(name), size: aws.ToInt64(head.ContentLength), mtime: aws.ToTime(head.LastModified)}, nil
	}
	if !isNotFound(err) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}
	dirPrefix := key + "/"
	out, err := r.s.c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: &r.s.bucket, Prefix: &dirPrefix, MaxKeys: aws.Int32(1)})
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}
	if len(out.Contents) > 0 {
		return &s3Dir{name: path.Base(name)}, nil
	}
	return nil, notExist
}

// s3Dir stands for a "directory": a key prefix with objects below it.
type s3Dir struct{ name string }

func (d *s3Dir) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.name, Err: errors.New("is a directory")}
}
func (d *s3Dir) Seek(int64, int) (int64, error) { return 0, errors.New("is a directory") }
func (d *s3Dir) Close() error                   { return nil }
func (d *s3Dir) Stat() (fs.FileInfo, error)     { return objInfo{name: d.name, dir: true}, nil }

// s3File reads an object with ranged GETs, so ServeContent can seek and answer Range
// requests without downloading the rest.
type s3File struct {
	r     *s3Root
	key   string
	name  string
	size  int64
	mtime time.Time

	pos    int64
	body   io.ReadCloser
	cancel context.CancelFunc // ends the GET that body belongs to
	// stalled is set by the watchdog when it cancels a read that waited too long.
	stalled atomic.Bool
}

func (f *s3File) Stat() (fs.FileInfo, error) {
	return objInfo{name: f.name, size: f.size, mtime: f.mtime}, nil
}

func (f *s3File) Read(p []byte) (int, error) {
	if f.pos >= f.size {
		return 0, io.EOF
	}
	// The GET gets its own context so the watchdog can cancel it without touching the
	// request's. The timer runs only while this call waits on the store.
	if f.body == nil {
		ctx, cancel := context.WithCancel(f.r.ctx)
		f.cancel = cancel
		f.stalled.Store(false)
		stop := f.watch()
		rng := fmt.Sprintf("bytes=%d-", f.pos)
		out, err := f.r.s.c.GetObject(ctx, &s3.GetObjectInput{Bucket: &f.r.s.bucket, Key: &f.key, Range: &rng})
		stop()
		if err != nil {
			f.release()
			return 0, f.readErr(err)
		}
		f.body = out.Body
	}
	stop := f.watch()
	n, err := f.body.Read(p)
	stop()
	f.pos += int64(n)
	if err != nil && err != io.EOF {
		return n, f.readErr(err)
	}
	return n, err
}

// watch cancels the current GET if the returned stop function is not called within
// readStallTimeout.
func (f *s3File) watch() (stop func()) {
	cancel := f.cancel
	t := time.AfterFunc(readStallTimeout, func() {
		f.stalled.Store(true)
		cancel()
	})
	return func() { t.Stop() }
}

func (f *s3File) readErr(err error) error {
	if f.stalled.Load() {
		return fmt.Errorf("s3: no data from %s for %v: %w", f.key, readStallTimeout, os.ErrDeadlineExceeded)
	}
	return err
}

// release closes the body, if any, and ends the GET's context.
func (f *s3File) release() {
	if f.body != nil {
		_ = f.body.Close()
		f.body = nil
	}
	if f.cancel != nil {
		f.cancel()
		f.cancel = nil
	}
}

func (f *s3File) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = f.pos + offset
	case io.SeekEnd:
		abs = f.size + offset
	default:
		return 0, errors.New("invalid whence")
	}
	if abs < 0 {
		return 0, errors.New("negative position")
	}
	if abs != f.pos {
		f.release()
	}
	f.pos = abs
	return abs, nil
}

func (f *s3File) Close() error {
	var err error
	if f.body != nil {
		err = f.body.Close()
		f.body = nil
	}
	if f.cancel != nil {
		f.cancel()
		f.cancel = nil
	}
	return err
}

// objInfo is the fs.FileInfo of an object or a prefix.
type objInfo struct {
	name  string
	size  int64
	mtime time.Time
	dir   bool
}

func (i objInfo) Name() string { return i.name }
func (i objInfo) Size() int64  { return i.size }
func (i objInfo) Mode() fs.FileMode {
	if i.dir {
		return fs.ModeDir | 0o555
	}
	return 0o444
}
func (i objInfo) ModTime() time.Time { return i.mtime }
func (i objInfo) IsDir() bool        { return i.dir }
func (i objInfo) Sys() any           { return nil }
