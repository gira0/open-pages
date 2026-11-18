package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gira0/open-pages/internal/names"
)

// The "domain" subcommand manages the custom domains of a site (see docs/custom-domains.md):
//
//	OPEN_PAGES_TOKEN=opt_... open-pages domain -server https://pages.corp add handbook handbook.corp.example
//
// It only changes which host names open-pages answers to. The reverse proxy in front still
// has to route the host to open-pages and hold its certificate.

const (
	domainUsageMsg = "usage: open-pages domain [-server URL] list <site>\n       open-pages domain [-server URL] add <site> <domain>\n       open-pages domain [-server URL] remove <site> <domain>"
	domainTimeout  = time.Minute
)

// Domain runs the domain subcommand and returns the process exit code.
func Domain(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := DomainCommand(ctx, args, os.Getenv, os.Stdout)
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
		return 0
	default:
		fmt.Fprintln(os.Stderr, "open-pages domain:", err)
		return 1
	}
}

// DomainCommand implements "open-pages domain". The token is read from the environment only.
func DomainCommand(ctx context.Context, args []string, getenv func(string) string, out io.Writer) error {
	fl := flag.NewFlagSet("domain", flag.ContinueOnError)
	server := fl.String("server", getenv(envServerURL), "server base URL (default $"+envServerURL+")")
	if err := fl.Parse(args); err != nil {
		return err
	}
	rest := fl.Args()
	if len(rest) < 2 || len(rest) > 3 {
		return errors.New(domainUsageMsg)
	}
	action, site := rest[0], rest[1]
	want := map[string]int{"list": 2, "add": 3, "remove": 3}[action]
	if want != len(rest) {
		return errors.New(domainUsageMsg)
	}
	if !names.ValidSite(site) {
		return fmt.Errorf("invalid site name %q: use a DNS label (lowercase letters, digits, hyphens)", site)
	}
	domain := ""
	if len(rest) == 3 {
		domain = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(rest[2]), "."))
		if !names.ValidDomain(domain) {
			return fmt.Errorf("invalid domain %q: use a lowercase host name such as handbook.corp.example", rest[2])
		}
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
	c := &deployClient{
		http: &http.Client{
			Timeout:       domainTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		base:  strings.TrimRight(base.String(), "/"),
		token: token,
	}
	path := "/v1/auth/sites/" + site + "/domains"
	switch action {
	case "list":
		rep, err := c.send(ctx, http.MethodGet, path, "", nil, 0)
		if err != nil {
			return err
		}
		if rep.status == http.StatusNotFound {
			return domainsNotFound(rep)
		}
		if rep.status != http.StatusOK {
			return rep.failure("listing the domains")
		}
		for _, d := range rep.Domains {
			fmt.Fprintln(out, d)
		}
		if len(rep.Domains) == 0 {
			fmt.Fprintf(out, "%s has no custom domains (allowed: %s)\n", site, strings.Join(rep.Suffixes, ", "))
		}
		return nil
	case "add":
		rep, err := c.send(ctx, http.MethodPut, path+"/"+url.PathEscape(domain), "", nil, 0)
		if err != nil {
			return err
		}
		switch rep.status {
		case http.StatusCreated:
			_, err = fmt.Fprintf(out, "added %s to %s\nThe reverse proxy must route this host to open-pages and serve a certificate for it.\n", domain, site)
			return err
		case http.StatusOK:
			_, err = fmt.Fprintf(out, "%s already has %s\n", site, domain)
			return err
		case http.StatusNotFound:
			return domainsNotFound(rep)
		default:
			return rep.failure("adding the domain")
		}
	default: // remove
		rep, err := c.send(ctx, http.MethodDelete, path+"/"+url.PathEscape(domain), "", nil, 0)
		if err != nil {
			return err
		}
		switch rep.status {
		case http.StatusOK:
			_, err = fmt.Fprintf(out, "removed %s from %s\n", domain, site)
			return err
		case http.StatusNotFound:
			return domainsNotFound(rep)
		default:
			return rep.failure("removing the domain")
		}
	}
}

// domainsNotFound explains a 404, which is also what a server with custom domains turned
// off answers (the routes do not exist), so the message names both causes.
func domainsNotFound(rep reply) error {
	if rep.Error != "" {
		return fmt.Errorf("%s (HTTP 404)", rep.Error)
	}
	return errors.New("not found: the server has custom domains turned off ([sites] custom_domains)")
}
