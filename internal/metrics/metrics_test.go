package metrics

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPrometheusOutput(t *testing.T) {
	r := New()
	r.ObserveHTTP("GET", "GET /healthz", 200)
	r.ObserveLoop("scheduler", time.Now().Add(-time.Millisecond), nil)
	r.ObserveLoop("scheduler", time.Now().Add(-time.Millisecond), errors.New("db"))
	var out bytes.Buffer
	r.WritePrometheus(&out, Gauges{DatabaseUp: true, Deployments: map[string]int64{"live": 2}, ReadyNodes: 1})
	for _, want := range []string{
		`composectl_database_up 1`,
		`composectl_http_requests_total{method="GET",route="GET /healthz",status="200"} 1`,
		`composectl_loop_runs_total{name="scheduler",result="success"} 1`,
		`composectl_loop_runs_total{name="scheduler",result="error"} 1`,
		`composectl_deployments{state="live"} 2`,
		`composectl_nodes_ready 1`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in:\n%s", want, out.String())
		}
	}
}
