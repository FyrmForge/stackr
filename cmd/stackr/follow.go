package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
			_, _ = io.WriteString(w, "stackr CLI is logged in. You can close this tab.\n")
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
