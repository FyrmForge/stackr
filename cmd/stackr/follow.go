package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// events reads a server-sent event stream, calling f per event until the
// stream ends or f says stop.
func events(r io.Reader, f func(name string, data []byte) (stop bool, err error)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 32<<20)
	var name string
	var data []byte
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if name != "" {
				if stop, err := f(name, data); stop || err != nil {
					return err
				}
			}
			name, data = "", nil
		case strings.HasPrefix(line, "event: "):
			name = line[len("event: "):]
		case strings.HasPrefix(line, "data: "):
			data = append(data, line[len("data: "):]...)
		}
	}
	return sc.Err()
}

// jobOut is the JSON of a job follow event.
type jobOut struct {
	Job struct {
		ID    string `json:"id"`
		State string `json:"state"`
		Error string `json:"error"`
	} `json:"job"`
	Log string `json:"log"`
}

// planFilter drops a job log's "plan:" lines, which the CLI printed as a
// table already. Log chunks can end mid-line, so it holds the tail back.
type planFilter struct {
	w   io.Writer
	buf []byte
}

func (f *planFilter) Write(p []byte) (int, error) {
	f.buf = append(f.buf, p...)
	for {
		i := bytes.IndexByte(f.buf, '\n')
		if i < 0 {
			return len(p), nil
		}
		if line := f.buf[:i+1]; !bytes.HasPrefix(line, []byte("plan: ")) {
			if _, err := f.w.Write(line); err != nil {
				return 0, err
			}
		}
		f.buf = f.buf[i+1:]
	}
}

func (f *planFilter) flush() {
	if !bytes.HasPrefix(f.buf, []byte("plan: ")) {
		_, _ = f.w.Write(f.buf)
	}
	f.buf = nil
}

// follow watches a job to its end: its log to stdout (stderr under
// --json), then the finished job. The exit reflects the job, not the
// request. Ctrl-C stops watching, and says the work goes on.
func (a *app) follow(base string, job any, what string) error {
	m, _ := job.(map[string]any)
	id, _ := m["id"].(string)
	if id == "" {
		return a.show(job)
	}
	res, err := a.request(http.MethodGet, base+"/jobs/"+id+"/events", nil)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	logw := a.out
	if a.json {
		logw = a.errw
	}
	if a.planShown && !a.json {
		pf := &planFilter{w: logw}
		defer pf.flush()
		logw = pf
	}
	var end jobOut
	var raw []byte
	err = events(res.Body, func(name string, data []byte) (bool, error) {
		var ev jobOut
		if name == "error" {
			var msg string
			_ = json.Unmarshal(data, &msg)
			return true, errors.New(msg)
		}
		if err := json.Unmarshal(data, &ev); err != nil {
			return true, err
		}
		_, _ = io.WriteString(logw, ev.Log)
		if name == "end" {
			end, raw = ev, data
			return true, nil
		}
		return false, nil
	})
	if a.ctx.Err() != nil {
		_, _ = fmt.Fprintf(a.errw, "stopped watching; the %s continues server-side (job %s)\n", what, id)
		return a.ctx.Err()
	}
	if err != nil {
		return err
	}
	if end.Job.ID == "" {
		return fmt.Errorf("the stream ended before job %s did; follow it with: stackr job log %s --follow", id, id)
	}
	if a.json {
		var v map[string]any
		_ = json.Unmarshal(raw, &v)
		_ = a.show(v["job"])
	}
	switch end.Job.State {
	case "done", "superseded":
		a.say("%s: %s", what, end.Job.State)
		return nil
	}
	if end.Job.Error != "" {
		return fmt.Errorf("%s %s: %s", what, end.Job.State, end.Job.Error)
	}
	return fmt.Errorf("%s %s", what, end.Job.State)
}

// browserLogin runs the loopback half of CLI login: open the panel's
// authorize page, wait for it to hand back a one-time code, return it.
// The key itself never goes through the browser; the code is exchanged
// by the CLI.
func (a *app) browserLogin(server, name string) (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer func() { _ = ln.Close() }()
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	state := hex.EncodeToString(b)
	port := ln.Addr().(*net.TCPAddr).Port
	u := fmt.Sprintf(
		"%s/cli/authorize?port=%d&state=%s&name=%s",
		strings.TrimRight(server, "/"),
		port,
		state,
		url.QueryEscape(name),
	)

	got := make(chan string, 1)
	srv := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			if q.Get("state") != state || q.Get("code") == "" {
				http.Error(w, "this login is not the one the CLI started", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = fmt.Fprintf(w, loggedInPage, html.EscapeString(strings.TrimRight(server, "/")))
			select {
			case got <- q.Get("code"):
			default:
			}
		}),
	}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	_, _ = fmt.Fprintf(a.errw, "Approve the login in your browser:\n  %s\n", u)
	openBrowser(u)
	ctx, cancel := context.WithTimeout(a.ctx, 5*time.Minute)
	defer cancel()
	select {
	case code := <-got:
		return code, nil
	case <-ctx.Done():
		return "", errors.New("no approval arrived; run stackr login again, or use --with-key")
	}
}

func openBrowser(u string) {
	cmd := "xdg-open"
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
	case "windows":
		cmd = "rundll32"
		u = "url.dll,FileProtocolHandler " + u
	}
	if os.Getenv("STACKR_NO_BROWSER") != "" {
		return
	}
	_ = exec.Command(cmd, strings.Fields(u)...).Start()
}

// loggedInPage is what the browser lands on once the CLI has its code: the
// panel's sign-in card (tokens from ui/css/input.css), self-contained since
// the CLI serves it. %[1]s is the server, for the logo.
const loggedInPage = `<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Logged in - stackr</title>
<link rel="icon" href="%[1]s/static/images/favicon-32.png">
<style>
:root{--bg:10 10 12;--surface:19 19 23;--border:38 38 46;--text:237 236 242;--muted:207 206 213;--success:63 179 127;color-scheme:dark}
@media (prefers-color-scheme:light){:root{--bg:246 245 250;--surface:255 255 255;--border:226 224 234;--text:27 26 34;--muted:94 91 109;color-scheme:light}}
body{margin:0;background:rgb(var(--bg));color:rgb(var(--text));font:14px/1.5 ui-sans-serif,system-ui,-apple-system,"Segoe UI",Roboto,Inter,sans-serif}
main{max-width:28rem;margin:0 auto;padding:6rem 1.5rem 0}
.brand{display:flex;gap:.625rem;align-items:center;justify-content:center;margin-bottom:2rem;font-size:1.25rem;font-weight:600}
.brand img{width:2.5rem;height:2.5rem}
.card{background:rgb(var(--surface));border:1px solid rgb(var(--border));border-radius:.75rem;padding:2rem;text-align:center;box-shadow:0 1px 2px rgba(0,0,0,.4),0 4px 16px rgba(0,0,0,.25)}
.ok{width:2.5rem;height:2.5rem;margin:0 auto 1rem;border-radius:999px;display:grid;place-items:center;background:rgb(var(--success)/.15);color:rgb(var(--success))}
h1{font-size:1.125rem;font-weight:600;margin:0 0 .25rem}
p{margin:0;color:rgb(var(--muted))}
@media (prefers-color-scheme:light){.card{box-shadow:0 1px 2px rgba(0,0,0,.05),0 4px 16px rgba(0,0,0,.06)}}
</style></head>
<body><main>
<div class="brand"><img src="%[1]s/static/images/logo-mark.png" alt="">stackr</div>
<section class="card">
<div class="ok"><svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M20 6 9 17l-5-5"/></svg></div>
<h1>The CLI is logged in</h1>
<p>You can close this tab and go back to your terminal.</p>
</section>
</main></body></html>
`
