package main

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
)

// The "deploy" subcommand zips a directory and uploads it as a new version of a site:
//
//	OPEN_PAGES_TOKEN=opt_... open-pages deploy -server https://pages.corp blog ./public

const (
	envToken       = "OPEN_PAGES_TOKEN"
	envServerURL   = "OPEN_PAGES_URL"
	deployTimeout  = 10 * time.Minute
	maxReplyBytes  = 1 << 20
	deployUsageMsg = "usage: open-pages deploy [-server URL] [-create=false] <site> <dir>"
)

// runDeploy runs the deploy subcommand and returns the process exit code.
func runDeploy(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := deployCommand(ctx, args, os.Getenv, os.Stdout)
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
		return 0
	default:
		fmt.Fprintln(os.Stderr, "open-pages deploy:", err)
		return 1
	}
}

// deployCommand implements "open-pages deploy". The token is read from the environment only,
// so it doesn't end up in shell history or process listings.
func deployCommand(ctx context.Context, args []string, getenv func(string) string, out io.Writer) error {
	fl := flag.NewFlagSet("deploy", flag.ContinueOnError)
	server := fl.String("server", getenv(envServerURL), "server base URL (default $"+envServerURL+")")
	create := fl.Bool("create", true, "create the site if it does not exist")
	if err := fl.Parse(args); err != nil {
		return err
	}
	if fl.NArg() != 2 {
		return errors.New(deployUsageMsg)
	}
	site, dir := fl.Arg(0), fl.Arg(1)

	if !validSiteName(site) {
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
		http:  &http.Client{Timeout: deployTimeout},
		base:  strings.TrimRight(base.String(), "/"),
		token: token,
	}
	rep, err := c.upload(ctx, site, archive, size)
	if err == nil && rep.status == http.StatusNotFound && *create {
		// Unknown site: create it, then upload again. A 409 means someone created it meanwhile.
		if err = c.createSite(ctx, site); err == nil {
			rep, err = c.upload(ctx, site, archive, size)
		}
	}
	if err != nil {
		return err
	}
	if rep.status != http.StatusOK {
		return rep.failure("upload")
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
	status  int
	Error   string `json:"error"`
	Version string `json:"version"`
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

func (c *deployClient) post(ctx context.Context, path, ctype string, body io.Reader, size int64) (reply, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, io.NopCloser(body))
	if err != nil {
		return reply{}, err
	}
	req.ContentLength = size
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", ctype)
	resp, err := c.http.Do(req)
	if err != nil {
		return reply{}, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	rep := reply{status: resp.StatusCode}
	// A body that isn't the expected JSON just leaves the fields empty.
	_ = json.NewDecoder(io.LimitReader(resp.Body, maxReplyBytes)).Decode(&rep)
	return rep, nil
}

func (c *deployClient) upload(ctx context.Context, site string, archive io.ReadSeeker, size int64) (reply, error) {
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return reply{}, err
	}
	return c.post(ctx, "/v1/auth/sites/"+site+"/upload", "application/zip", archive, size)
}

func (c *deployClient) createSite(ctx context.Context, site string) error {
	body, err := json.Marshal(siteCreate{Name: site})
	if err != nil {
		return err
	}
	rep, err := c.post(ctx, "/v1/auth/sites", "application/json", bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return err
	}
	if rep.status != http.StatusCreated && rep.status != http.StatusConflict {
		return rep.failure("creating the site")
	}
	return nil
}
