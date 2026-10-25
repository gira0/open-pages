package metrics

import (
	"strings"
	"testing"
	"time"
)

func TestMethodLabelBoundsCardinality(t *testing.T) {
	if MethodLabel("GET") != "GET" || MethodLabel("BREW") != "OTHER" {
		t.Error("MethodLabel must keep known methods and fold the rest")
	}
}

func TestWrite(t *testing.T) {
	c := New()
	c.ObserveRequest("GET", 200, 10*time.Millisecond)
	c.ObserveRequest("OTHER", 503, 20*time.Second)
	c.RecordDeploy(DeployRejected)

	var b strings.Builder
	c.Write(&b)
	out := b.String()
	for _, want := range []string{
		`openpages_http_requests_total{method="GET",code="2xx"} 1`,
		`openpages_http_requests_total{method="OTHER",code="5xx"} 1`,
		`openpages_http_request_duration_seconds_bucket{le="0.025"} 1`,
		`openpages_http_request_duration_seconds_bucket{le="+Inf"} 2`,
		`openpages_deploys_total{result="rejected"} 1`,
		`openpages_errors_total 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}
