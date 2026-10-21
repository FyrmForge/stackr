package metrics

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The wire shapes are VictoriaMetrics v1.154's, taken from a real run.
func TestVictoria(t *testing.T) {
	var imported []string
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/import":
			b, _ := io.ReadAll(r.Body)
			imported = strings.Split(strings.TrimSpace(string(b)), "\n")
			w.WriteHeader(http.StatusNoContent)
		case "/api/v1/query_range":
			q := r.URL.Query()
			query = q.Get("query") + " " + q.Get("step") + " nocache=" + q.Get("nocache") + " " + q.Get("start") + "-" + q.Get("end")
			_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"matrix","result":[`+
				`{"metric":{"__name__":"stackr_cpu_pct","key":"node:abc","node":"abc"},"values":[[1792579060,"10"],[1792579120,"25"]]},`+
				`{"metric":{"__name__":"stackr_4xx_s","key":"node:abc","node":"abc"},"values":[[1792579060,"100"],[1792579120,"NaN"]]}]}}`)
		case "/metrics":
			_, _ = io.WriteString(w, "vm_data_size_bytes{type=\"storage/inmemory\"} 16384\nvm_data_size_bytes{type=\"indexdb/file\"} 72\nvm_rows 9\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	v := &Victoria{URL: srv.URL, Labels: func(_ context.Context, key string) map[string]string {
		return map[string]string{"node": strings.TrimPrefix(key, "node:")}
	}}
	ctx := context.Background()
	t0 := time.UnixMilli(1792579000000)
	err := v.Write(ctx, []Point{
		{Key: "node:abc", Name: "cpu_pct", T: t0, V: 10},
		{Key: "node:abc", Name: "4xx_s", T: t0, V: 100},
		{Key: "node:abc", Name: "cpu_pct", T: t0.Add(30 * time.Second), V: 20},
		{Key: "node:abc", Name: "cpu_pct", T: t0.Add(time.Minute), V: math.NaN()},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(imported) != 2 {
		t.Fatalf("lines %q", imported)
	}
	var l vmLine
	if err := json.Unmarshal([]byte(imported[0]), &l); err != nil {
		t.Fatal(err)
	}
	if l.Metric["__name__"] != "stackr_cpu_pct" || l.Metric["key"] != "node:abc" || l.Metric["node"] != "abc" ||
		len(l.Values) != 2 || l.Timestamps[1] != 1792579030000 {
		t.Errorf("line %+v", l)
	}

	got, err := v.Query(ctx, "node:abc", t0, t0.Add(90*time.Second), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// start moves to a whole number of steps before end (90s -> 60s).
	if query != `avg_over_time({key="node:abc"}[60s]) keep_metric_names 60s nocache=1 1792579030-1792579090` {
		t.Errorf("query %q", query)
	}
	cpu := got["cpu_pct"]
	if len(cpu) != 2 || cpu[1].V != 25 || !cpu[0].T.Equal(time.Unix(1792579000, 0)) {
		t.Errorf("cpu %+v", cpu)
	}
	if len(got["4xx_s"]) != 1 { // NaN left out
		t.Errorf("4xx %+v", got["4xx_s"])
	}

	n, err := v.DataSize(ctx)
	if err != nil || n != 16384+72 {
		t.Errorf("size %d %v", n, err)
	}

	bad := &Victoria{URL: srv.URL + "/nope"}
	if _, err := bad.Query(ctx, "k", t0, t0, time.Minute); err == nil {
		t.Error("a 404 is an error")
	}
}
