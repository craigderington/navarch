// Package metrics provides the control plane's small Prometheus-compatible
// registry without adding a monitoring dependency to the binary.
package metrics

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"sync"
	"time"
)

type Registry struct {
	mu       sync.Mutex
	http     map[httpKey]uint64
	loops    map[loopKey]uint64
	loopLast map[string]float64
}

type httpKey struct{ Method, Route, Status string }
type loopKey struct{ Name, Result string }

func New() *Registry {
	return &Registry{http: map[httpKey]uint64{}, loops: map[loopKey]uint64{}, loopLast: map[string]float64{}}
}

func (r *Registry) ObserveHTTP(method, route string, status int) {
	r.mu.Lock()
	r.http[httpKey{method, route, strconv.Itoa(status)}]++
	r.mu.Unlock()
}

func (r *Registry) ObserveLoop(name string, started time.Time, err error) {
	result := "success"
	if err != nil {
		result = "error"
	}
	r.mu.Lock()
	r.loops[loopKey{name, result}]++
	r.loopLast[name] = time.Since(started).Seconds()
	r.mu.Unlock()
}

type Gauges struct {
	DatabaseUp       bool
	Deployments      map[string]int64
	ReadyNodes       int64
	ActivePreviews   int64
	RecentTombstones int64
}

func (r *Registry) WritePrometheus(w io.Writer, g Gauges) {
	r.mu.Lock()
	httpCounts := make(map[httpKey]uint64, len(r.http))
	for k, v := range r.http {
		httpCounts[k] = v
	}
	loopCounts := make(map[loopKey]uint64, len(r.loops))
	for k, v := range r.loops {
		loopCounts[k] = v
	}
	loopLast := make(map[string]float64, len(r.loopLast))
	for k, v := range r.loopLast {
		loopLast[k] = v
	}
	r.mu.Unlock()

	fmt.Fprintln(w, "# HELP composectl_database_up Whether the control-plane database query succeeded.")
	fmt.Fprintln(w, "# TYPE composectl_database_up gauge")
	db := 0
	if g.DatabaseUp {
		db = 1
	}
	fmt.Fprintf(w, "composectl_database_up %d\n", db)

	writeHTTPCounters(w, httpCounts)
	writeLoopMetrics(w, loopCounts, loopLast)
	fmt.Fprintln(w, "# HELP composectl_deployments Current deployments by state.")
	fmt.Fprintln(w, "# TYPE composectl_deployments gauge")
	states := make([]string, 0, len(g.Deployments))
	for state := range g.Deployments {
		states = append(states, state)
	}
	sort.Strings(states)
	for _, state := range states {
		fmt.Fprintf(w, "composectl_deployments{state=%q} %d\n", state, g.Deployments[state])
	}
	fmt.Fprintln(w, "# HELP composectl_nodes_ready Nodes eligible for placement.")
	fmt.Fprintln(w, "# TYPE composectl_nodes_ready gauge")
	fmt.Fprintf(w, "composectl_nodes_ready %d\n", g.ReadyNodes)
	fmt.Fprintln(w, "# HELP composectl_previews_active Unexpired preview environments.")
	fmt.Fprintln(w, "# TYPE composectl_previews_active gauge")
	fmt.Fprintf(w, "composectl_previews_active %d\n", g.ActivePreviews)
	fmt.Fprintln(w, "# HELP composectl_tombstones_recent Teardown instructions still offered to agents.")
	fmt.Fprintln(w, "# TYPE composectl_tombstones_recent gauge")
	fmt.Fprintf(w, "composectl_tombstones_recent %d\n", g.RecentTombstones)
}

func writeHTTPCounters(w io.Writer, counts map[httpKey]uint64) {
	fmt.Fprintln(w, "# HELP composectl_http_requests_total HTTP requests by method, route, and status.")
	fmt.Fprintln(w, "# TYPE composectl_http_requests_total counter")
	keys := make([]httpKey, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Route != keys[j].Route {
			return keys[i].Route < keys[j].Route
		}
		if keys[i].Method != keys[j].Method {
			return keys[i].Method < keys[j].Method
		}
		return keys[i].Status < keys[j].Status
	})
	for _, k := range keys {
		fmt.Fprintf(w, "composectl_http_requests_total{method=%q,route=%q,status=%q} %d\n", k.Method, k.Route, k.Status, counts[k])
	}
}

func writeLoopMetrics(w io.Writer, counts map[loopKey]uint64, last map[string]float64) {
	fmt.Fprintln(w, "# HELP composectl_loop_runs_total Control-plane loop ticks by result.")
	fmt.Fprintln(w, "# TYPE composectl_loop_runs_total counter")
	keys := make([]loopKey, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Name != keys[j].Name {
			return keys[i].Name < keys[j].Name
		}
		return keys[i].Result < keys[j].Result
	})
	for _, k := range keys {
		fmt.Fprintf(w, "composectl_loop_runs_total{name=%q,result=%q} %d\n", k.Name, k.Result, counts[k])
	}
	fmt.Fprintln(w, "# HELP composectl_loop_last_duration_seconds Duration of the latest loop tick.")
	fmt.Fprintln(w, "# TYPE composectl_loop_last_duration_seconds gauge")
	names := make([]string, 0, len(last))
	for name := range last {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(w, "composectl_loop_last_duration_seconds{name=%q} %g\n", name, last[name])
	}
}
