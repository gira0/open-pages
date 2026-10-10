package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// Metrics are hand-rolled in the Prometheus text exposition format rather than pulled in
// from prometheus/client_golang: a few counters and one histogram don't justify a
// dependency tree that govulncheck and dependency review would have to keep vetting.
// Everything is a fixed-size array of atomics, so recording is lock-free and label
// cardinality is bounded by construction: requests are labelled by method (from a fixed
// list) and status class, never by path or site.

var metricMethods = [...]string{"GET", "HEAD", "POST", "PUT", "DELETE", "OPTIONS", "PATCH", "OTHER"}

// methodLabel maps a request method onto metricMethods, folding anything unusual into OTHER.
func methodLabel(m string) string {
	for _, known := range metricMethods {
		if m == known {
			return m
		}
	}
	return "OTHER"
}

// durationBounds are the histogram bucket upper bounds in seconds.
var durationBounds = [...]float64{0.005, 0.025, 0.1, 0.25, 1, 2.5, 10}

// deployResult is the outcome of a deploy, as labelled in openpages_deploys_total.
type deployResult int

const (
	deployOK       deployResult = iota // new version is live
	deployRejected                     // the archive was refused (bad, too large, unsafe)
	deployFailed                       // anything else, including a site deleted mid-deploy
	deployResultCount
)

var deployResultNames = [deployResultCount]string{"ok", "rejected", "failed"}

type metrics struct {
	requests    [len(metricMethods)][5]atomic.Uint64 // by method, status class 1xx..5xx
	durBuckets  [len(durationBounds) + 1]atomic.Uint64
	durSumNanos atomic.Int64
	deploys     [deployResultCount]atomic.Uint64
	errors      atomic.Uint64 // responses with a 5xx status
}

func newMetrics() *metrics { return &metrics{} }

// observeRequest records one finished request; method must come from methodLabel.
func (m *metrics) observeRequest(method string, status int, d time.Duration) {
	mi := len(metricMethods) - 1 // OTHER
	for i, name := range metricMethods {
		if name == method {
			mi = i
		}
	}
	class := min(max(status/100, 1), 5) - 1
	m.requests[mi][class].Add(1)
	if status >= http.StatusInternalServerError {
		m.errors.Add(1)
	}

	sec := d.Seconds()
	bi := len(durationBounds)
	for i, bound := range durationBounds {
		if sec <= bound {
			bi = i
			break
		}
	}
	m.durBuckets[bi].Add(1)
	m.durSumNanos.Add(int64(max(d, 0)))
}

func (m *metrics) recordDeploy(r deployResult) { m.deploys[r].Add(1) }

// write renders the metrics in the Prometheus text format (version 0.0.4).
func (m *metrics) write(w io.Writer) {
	fmt.Fprint(w, "# HELP openpages_http_requests_total HTTP requests served, by method and status class.\n")
	fmt.Fprint(w, "# TYPE openpages_http_requests_total counter\n")
	for mi, method := range metricMethods {
		for ci := range m.requests[mi] {
			fmt.Fprintf(w, "openpages_http_requests_total{method=%q,code=\"%dxx\"} %d\n",
				method, ci+1, m.requests[mi][ci].Load())
		}
	}

	fmt.Fprint(w, "# HELP openpages_http_request_duration_seconds HTTP request duration.\n")
	fmt.Fprint(w, "# TYPE openpages_http_request_duration_seconds histogram\n")
	var cum uint64
	for i, bound := range durationBounds {
		cum += m.durBuckets[i].Load()
		fmt.Fprintf(w, "openpages_http_request_duration_seconds_bucket{le=\"%g\"} %d\n", bound, cum)
	}
	cum += m.durBuckets[len(durationBounds)].Load()
	fmt.Fprintf(w, "openpages_http_request_duration_seconds_bucket{le=\"+Inf\"} %d\n", cum)
	fmt.Fprintf(w, "openpages_http_request_duration_seconds_sum %g\n", float64(m.durSumNanos.Load())/1e9)
	fmt.Fprintf(w, "openpages_http_request_duration_seconds_count %d\n", cum)

	fmt.Fprint(w, "# HELP openpages_deploys_total Site deploys, by result.\n")
	fmt.Fprint(w, "# TYPE openpages_deploys_total counter\n")
	for i, name := range deployResultNames {
		fmt.Fprintf(w, "openpages_deploys_total{result=%q} %d\n", name, m.deploys[i].Load())
	}

	fmt.Fprint(w, "# HELP openpages_errors_total Responses with a 5xx status.\n")
	fmt.Fprint(w, "# TYPE openpages_errors_total counter\n")
	fmt.Fprintf(w, "openpages_errors_total %d\n", m.errors.Load())
}

// metricsAuthorized reports whether r may read /metrics: always when no token is
// configured, otherwise only with a matching "Authorization: Bearer <token>" header.
func (s *Server) metricsAuthorized(r *http.Request) bool {
	if s.cfg.MetricsToken == "" {
		return true
	}
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return false
	}
	// Hash first so the comparison is constant-time whatever the lengths are.
	want := sha256.Sum256([]byte(s.cfg.MetricsToken))
	have := sha256.Sum256([]byte(got))
	return subtle.ConstantTimeCompare(want[:], have[:]) == 1
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if !s.metricsAuthorized(r) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeError(w, http.StatusUnauthorized, "metrics token required")
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	s.metrics.write(w)
}

// metricsServer returns the dedicated metrics listener, or nil when [metrics] listen is
// empty. It serves only /metrics and is wrapped in the same request logging.
func (s *Server) metricsServer() *http.Server {
	if s.cfg.MetricsListen == "" {
		return nil
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	return &http.Server{
		Addr:              s.cfg.MetricsListen,
		Handler:           s.observe(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}
}
