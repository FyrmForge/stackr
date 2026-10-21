package metrics

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Victoria is a VictoriaMetrics single node: points go to its JSON import
// API, a series is named after the measure and labelled with its key plus
// what Labels adds (node, org, stack, env, tile).
type Victoria struct {
	URL    string       // base URL, e.g. http://observe
	Client *http.Client // nil = http.DefaultClient
	// Labels are a key's extra labels; nil = the key only.
	Labels func(ctx context.Context, key string) map[string]string
}

func (v *Victoria) client() *http.Client {
	if v.Client == nil {
		return http.DefaultClient
	}
	return v.Client
}

func (v *Victoria) do(req *http.Request) ([]byte, error) {
	res, err := v.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	if res.StatusCode/100 != 2 {
		return nil, fmt.Errorf("victoriametrics %s: %s: %s", req.URL.Path, res.Status, bytes.TrimSpace(b))
	}
	return b, nil
}

type vmLine struct {
	Metric     map[string]string `json:"metric"`
	Values     []float64         `json:"values"`
	Timestamps []int64           `json:"timestamps"`
}

func (v *Victoria) Write(ctx context.Context, ps []Point) error {
	if len(ps) == 0 {
		return nil
	}
	lines := map[[2]string]*vmLine{}
	var order [][2]string
	extra := map[string]map[string]string{}
	for _, p := range ps {
		id := [2]string{p.Key, p.Name}
		l := lines[id]
		if l == nil {
			if _, ok := extra[p.Key]; !ok && v.Labels != nil {
				extra[p.Key] = v.Labels(ctx, p.Key)
			}
			m := map[string]string{}
			for k, x := range extra[p.Key] {
				m[k] = x
			}
			m["__name__"], m["key"] = p.Name, p.Key
			l = &vmLine{Metric: m}
			lines[id] = l
			order = append(order, id)
		}
		l.Values = append(l.Values, p.V)
		l.Timestamps = append(l.Timestamps, p.T.UnixMilli())
	}
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	for _, id := range order {
		if err := enc.Encode(lines[id]); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.URL+"/api/v1/import", &body)
	if err != nil {
		return err
	}
	_, err = v.do(req)
	return err
}

// Query averages each measure of key per step, as Memory does: a bucket is
// stamped with its start.
func (v *Victoria) Query(ctx context.Context, key string, from, to time.Time, step time.Duration) (map[string][]Sample, error) {
	if step < time.Second {
		step = time.Second
	}
	s := strconv.FormatInt(int64(step/time.Second), 10) + "s"
	q := url.Values{
		"query": {"avg_over_time({key=" + strconv.Quote(key) + "}[" + s + "]) keep_metric_names"},
		"start": {strconv.FormatInt(from.Unix(), 10)},
		"end":   {strconv.FormatInt(to.Unix(), 10)},
		"step":  {s},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.URL+"/api/v1/query_range?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	b, err := v.do(req)
	if err != nil {
		return nil, err
	}
	var res struct {
		Data struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Values [][2]any          `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &res); err != nil {
		return nil, err
	}
	out := map[string][]Sample{}
	for _, r := range res.Data.Result {
		name := r.Metric["__name__"]
		for _, p := range r.Values {
			ts, _ := p[0].(float64)
			x, _ := p[1].(string)
			f, err := strconv.ParseFloat(x, 64)
			if err != nil || math.IsNaN(f) || math.IsInf(f, 0) { // JSON has no NaN
				continue
			}
			// The value at ts averages (ts-step, ts].
			at := time.UnixMilli(int64(ts * 1000)).Add(-step)
			out[name] = append(out[name], Sample{T: at, V: f})
		}
	}
	return out, nil
}

// DataSize is the bytes VictoriaMetrics holds, its own vm_data_size_bytes
// summed over every part.
func (v *Victoria) DataSize(ctx context.Context) (uint64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.URL+"/metrics", nil)
	if err != nil {
		return 0, err
	}
	b, err := v.do(req)
	if err != nil {
		return 0, err
	}
	var sum float64
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		l := sc.Text()
		if !strings.HasPrefix(l, "vm_data_size_bytes{") && !strings.HasPrefix(l, "vm_data_size_bytes ") {
			continue
		}
		if f, err := strconv.ParseFloat(l[strings.LastIndexByte(l, ' ')+1:], 64); err == nil {
			sum += f
		}
	}
	return uint64(sum), sc.Err()
}
