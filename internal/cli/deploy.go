// Package cli implements the "open-pages deploy" client subcommand: it zips a directory and
// uploads it to a running server over the HTTP API.
package cli

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gira0/open-pages/internal/names"
)

// The "deploy" subcommand zips a directory and uploads it as a new version of a site:
//
//	OPEN_PAGES_TOKEN=opt_... open-pages deploy -server https://pages.corp blog ./public
//
// With -visibility, the site is created with that visibility or its visibility is changed:
// before the upload when the change narrows who can see the site, after a successful upload
// when it widens it.

const (
	envToken       = "OPEN_PAGES_TOKEN"
	envServerURL   = "OPEN_PAGES_URL"
	deployTimeout  = 10 * time.Minute
	maxReplyBytes  = 1 << 20
	deployUsageMsg = "usage: open-pages deploy [-server URL] [-create=false] [-visibility V] <site> <dir>"
)

// Deploy runs the deploy subcommand and returns the process exit code.
func Deploy(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := DeployCommand(ctx, args, os.Getenv, os.Stdout)
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
		return 0
	default:
		fmt.Fprintln(os.Stderr, "open-pages deploy:", err)
		return 1
	}
}

// DeployCommand implements "open-pages deploy". The token is read from the environment only,
// so it doesn't end up in shell history or process listings.
func DeployCommand(ctx context.Context, args []string, getenv func(string) string, out io.Writer) error {
	fl := flag.NewFlagSet("deploy", flag.ContinueOnError)
	server := fl.String("server", getenv(envServerURL), "server base URL (default $"+envServerURL+")")
	create := fl.Bool("create", true, "create the site if it does not exist")
	visibility := fl.String("visibility", "", `set the site's visibility: "public", "authenticated" or "restricted" (default: leave as is; new sites are public)`)
	if err := fl.Parse(args); err != nil {
		return err
	}
	if fl.NArg() != 2 {
		return errors.New(deployUsageMsg)
	}
	site, dir := fl.Arg(0), fl.Arg(1)
	if *visibility != "" && !validVisibility(*visibility) {
		return errors.New(errBadVisibility)
	}

	if !names.ValidSite(site) {
		return fmt.Errorf("invalid site name %q: use a DNS label (lowercase letters, digits, hyphens)", site)
	}
	token := strings.TrimSpace(getenv(envToken))
	if token == "" {
		return errors.New(envToken + " is not set")
	}
	base, err := parseServerURL(*server)
	if err != nil {
		return err
	}
	if base.Scheme == "http" && !isLoopbackHost(base.Hostname()) {
		fmt.Fprintln(os.Stderr, "warning: the server URL is plain http, so the token is sent unencrypted")
	}

	archive, size, err := zipToTemp(dir)
	if err != nil {
		return err
	}
	defer func() {
		_ = archive.Close()
		_ = os.Remove(archive.Name())
	}()

	c := &deployClient{
		http: &http.Client{
			Timeout: deployTimeout,
			// Never follow redirects: they could carry the bearer token to another origin.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		base:  strings.TrimRight(base.String(), "/"),
		token: token,
	}
	// A visibility change that narrows who can see the site is made before the upload, and
	// one that widens it only after the upload succeeded, so neither the old nor the new
	// version is served more widely than asked, even if the upload fails. These are separate
	// requests, not one atomic step: concurrent deploys to the same site can interleave.
	created, widen := false, false
	if *visibility != "" {
		if created, widen, err = c.prepareVisibility(ctx, site, *visibility, *create); err != nil {
			return err
		}
	}
	rep, err := c.upload(ctx, site, archive, size)
	if err == nil && rep.status == http.StatusNotFound && *create && !created {
		// Unknown site: create it, then upload again. A 409 means someone created it meanwhile.
		if err = c.createSite(ctx, site, *visibility); err == nil {
			rep, err = c.upload(ctx, site, archive, size)
		}
	}
	if err != nil {
		return err
	}
	if rep.status != http.StatusOK {
		return rep.failure("upload")
	}
	if widen {
		if err := c.setVisibility(ctx, site, *visibility); err != nil {
			return fmt.Errorf("deployed %s (version %s), but %w", site, rep.Version, err)
		}
	}
	_, err = fmt.Fprintf(out, "deployed %s (version %s, %d bytes)\n", site, rep.Version, size)
	return err
}

func parseServerURL(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, errors.New("server URL is required: use -server or $" + envServerURL)
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("server URL must look like https://pages.example.com")
	}
	return u, nil
}

func isLoopbackHost(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// zipToTemp zips dir into a temporary file positioned at its start and returns it with its size.
func zipToTemp(dir string) (*os.File, int64, error) {
	f, err := os.CreateTemp("", "open-pages-deploy-*.zip")
	if err != nil {
		return nil, 0, fmt.Errorf("create temp file: %w", err)
	}
	fail := func(err error) (*os.File, int64, error) {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, 0, err
	}
	if err := zipDir(f, dir); err != nil {
		return fail(err)
	}
	size, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return fail(err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fail(err)
	}
	return f, size, nil
}

// zipDir writes the contents of dir to w as a zip archive. Entry names are slash-separated
// and relative to dir. Files are read through os.Root, so nothing outside dir is ever
// opened, and anything but regular files and directories (symlinks, devices) is an error.
func zipDir(w io.Writer, dir string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("open %s: %w", dir, err)
	}
	defer root.Close()

	zw := zip.NewWriter(w)
	files := 0
	err = fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		if !fs.ValidPath(name) {
			return fmt.Errorf("unsafe path %q", name)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		hdr, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		hdr.Name = name
		switch {
		case d.IsDir():
			hdr.Name += "/"
			_, err = zw.CreateHeader(hdr)
			return err
		case d.Type().IsRegular():
			hdr.Method = zip.Deflate
			dst, err := zw.CreateHeader(hdr)
			if err != nil {
				return err
			}
			src, err := root.Open(name)
			if err != nil {
				return err
			}
			defer src.Close()
			if _, err := io.Copy(dst, src); err != nil {
				return err
			}
			files++
			return nil
		default:
			return fmt.Errorf("%s is not a regular file or directory (symlinks are not deployed)", name)
		}
	})
	if err != nil {
		return err
	}
	if files == 0 {
		return fmt.Errorf("%s contains no files", dir)
	}
	return zw.Close()
}

// reply is the part of an API response the CLI cares about.
type reply struct {
	status     int
	Error      string `json:"error"`
	Version    string `json:"version"`
	Visibility string `json:"visibility"`
}

func (r reply) failure(what string) error {
	msg := r.Error
	if msg == "" {
		msg = http.StatusText(r.status)
	}
	hint := ""
	if r.status == http.StatusUnauthorized {
		hint = " (check " + envToken + ")"
	}
	return fmt.Errorf("%s failed: %s (HTTP %d)%s", what, msg, r.status, hint)
}

type deployClient struct {
	http  *http.Client
	base  string
	token string
}

func (c *deployClient) send(ctx context.Context, method, path, ctype string, body io.Reader, size int64) (reply, error) {
	var rc io.ReadCloser = http.NoBody
	if body != nil {
		rc = io.NopCloser(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rc)
	if err != nil {
		return reply{}, err
	}
	req.ContentLength = size
	req.Header.Set("Authorization", "Bearer "+c.token)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return reply{}, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return reply{}, fmt.Errorf("the server redirected the request (HTTP %d); redirects are not followed so the token is never forwarded, use the final URL with -server", resp.StatusCode)
	}
	rep := reply{status: resp.StatusCode}
	// A body that isn't the expected JSON just leaves the fields empty.
	_ = json.NewDecoder(io.LimitReader(resp.Body, maxReplyBytes)).Decode(&rep)
	return rep, nil
}

func (c *deployClient) upload(ctx context.Context, site string, archive io.ReadSeeker, size int64) (reply, error) {
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return reply{}, err
	}
	return c.send(ctx, http.MethodPost, "/v1/auth/sites/"+site+"/upload", "application/zip", archive, size)
}

func (c *deployClient) sendJSON(ctx context.Context, method, path string, v any) (reply, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return reply{}, err
	}
	return c.send(ctx, method, path, "application/json", bytes.NewReader(body), int64(len(body)))
}

// createSite creates site with the given visibility ("" for the server's default). A 409
// means someone created it meanwhile, which is fine.
func (c *deployClient) createSite(ctx context.Context, site, visibility string) error {
	rep, err := c.sendJSON(ctx, http.MethodPost, "/v1/auth/sites", siteCreateBody{Name: site, Visibility: visibility})
	if err != nil {
		return err
	}
	if rep.status != http.StatusCreated && rep.status != http.StatusConflict {
		return rep.failure("creating the site")
	}
	return nil
}

// openness ranks visibilities from the narrowest audience to the widest.
var openness = map[string]int{"restricted": 0, "authenticated": 1, "public": 2}

const errBadVisibility = `visibility must be "public", "authenticated" or "restricted"`

// validVisibility reports whether v is a visibility the server knows.
func validVisibility(v string) bool {
	_, ok := openness[v]
	return ok
}

// siteCreateBody is the body of POST /v1/auth/sites.
type siteCreateBody struct {
	Name       string `json:"name"`
	Visibility string `json:"visibility"`
}

// siteUpdateBody is the body of PUT /v1/auth/sites/{name}; absent fields are left unchanged.
type siteUpdateBody struct {
	Visibility *string `json:"visibility"`
}

// prepareVisibility gets site ready to be deployed with the given visibility. A missing site
// is created with it when create is set (created reports that). An existing site's
// visibility is changed now if that narrows its audience; if it widens it, widen reports that
// the change must wait until the upload has succeeded.
func (c *deployClient) prepareVisibility(ctx context.Context, site, visibility string, create bool) (created, widen bool, err error) {
	rep, err := c.send(ctx, http.MethodGet, "/v1/auth/sites/"+site, "", nil, 0)
	if err != nil {
		return false, false, err
	}
	switch {
	case rep.status == http.StatusOK:
		if openness[visibility] > openness[rep.Visibility] {
			return false, true, nil
		}
		if visibility == rep.Visibility {
			return false, false, nil
		}
		return false, false, c.setVisibility(ctx, site, visibility)
	case rep.status == http.StatusNotFound && create:
		// If someone else created it meanwhile (409), the upload that follows is refused,
		// because only the owner may deploy.
		return true, false, c.createSite(ctx, site, visibility)
	case rep.status == http.StatusNotFound:
		return false, false, nil // the upload reports the missing site
	default:
		return false, false, rep.failure("reading the site")
	}
}

// setVisibility sets the visibility of an existing site.
func (c *deployClient) setVisibility(ctx context.Context, site, visibility string) error {
	rep, err := c.sendJSON(ctx, http.MethodPut, "/v1/auth/sites/"+site, siteUpdateBody{Visibility: &visibility})
	if err != nil {
		return err
	}
	if rep.status != http.StatusOK {
		return rep.failure("setting the visibility")
	}
	return nil
}
