package main

import (
	"io"
	"strings"

	"github.com/spf13/cobra"
)

// alerts is the alert webhook (admin): the noun under `stackr admin`.
func (a *app) alerts() *cobra.Command {
	var u string
	var secretStdin, noSecret bool
	show := leaf("show", "admin.alerts", "Show the alert webhook and its last delivery", exact(0), func(*cobra.Command, []string) error {
		v, err := a.call(GET, "/admin/alerts", nil)
		if err != nil || a.json {
			if err == nil {
				err = a.show(v)
			}
			return err
		}
		m, _ := v.(map[string]any)
		out := map[string]any{"url": cell(m["url"]), "secret": "not set", "last delivery": "none yet"}
		if m["url"] == "" {
			out["url"] = "not set (alerts off)"
		}
		if m["secret_set"] == true {
			out["secret"] = "set"
		}
		if d, ok := m["last_delivery"].(map[string]any); ok {
			s := "failed " + cell(d["reason"])
			if d["ok"] == true {
				s = "ok"
			}
			out["last delivery"] = s + " " + a.stamp(cell(d["at"]), stampLayout)
		}
		return a.show(out, "url", "secret", "last delivery")
	})
	set := leaf("set", "admin.alerts-set", "Set the alert webhook URL and its signing secret", exact(0), func(*cobra.Command, []string) error {
		body := map[string]any{"url": u}
		switch {
		case secretStdin && noSecret:
			return usage("--secret-stdin and --no-secret do not go together")
		case secretStdin:
			b, err := io.ReadAll(a.in)
			if err != nil {
				return err
			}
			s := strings.TrimRight(string(b), "\r\n")
			if s == "" {
				return usage("--secret-stdin: stdin is empty")
			}
			body["secret"] = s
		case noSecret:
			body["secret"] = ""
		}
		_, err := a.call(PUT, "/admin/alerts", body)
		return err
	})
	set.Flags().StringVar(&u, "url", "", "where alerts are posted (http or https); empty keeps the current one")
	set.Flags().BoolVar(&secretStdin, "secret-stdin", false, "read the HMAC signing secret from stdin")
	set.Flags().BoolVar(&noSecret, "no-secret", false, "stop signing alerts")
	test := leaf("test", "admin.alerts-test", "Send a test alert now", exact(0), func(*cobra.Command, []string) error {
		if _, err := a.call(POST, "/admin/alerts/test", nil); err != nil {
			return err
		}
		return a.show("Test alert delivered.")
	})
	clear := leaf("clear", "admin.alerts-clear", "Turn alerts off: forget the URL and secret", exact(0), func(*cobra.Command, []string) error {
		_, err := a.call(DELETE, "/admin/alerts", nil)
		return err
	})
	return noun("alerts", "Alert webhook: node offline, failed backups, expiring certs, low disk (admin)", show, set, test, clear)
}
