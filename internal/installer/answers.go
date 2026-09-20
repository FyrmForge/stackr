// Package installer is stackr-install: the form, the checks on every answer,
// and the docker commands that create the panel on a fresh host
// (docs/plans/49-installer-binary.md).
package installer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/netaddr"
)

// Answers is everything the install needs from the operator. The form and
// the flags both fill it; Check is the one place that decides it is usable.
// A domain is not optional: apps are reached by name through traefik, and so
// is the panel. An install on a bare IP would have no names, no certificates
// and no route to the panel but a published port, which is a different
// product, not a flag.
type Answers struct {
	// Apps get names under Root (app.stack.org.example.com), the panel its
	// own Host under it. Blank Host means stkr.<Root>.
	Root       string
	Host       string
	Cloudflare bool
	Proxies    string
	HTTPS      bool
	Email      string
	HTTPPort   string
	HTTPSPort  string
	DataDir    string
}

// PanelHost is the host the panel answers on.
func (a *Answers) PanelHost() string {
	if a.Host == "" {
		return "stkr." + a.Root
	}
	return a.Host
}

// TLS reports whether Let's Encrypt is on. Off for a name the ACME challenge
// cannot reach, or when something in front already terminates HTTPS.
func (a *Answers) TLS() bool { return a.HTTPS }

// BaseURL is how the operator reaches the panel after the install.
func (a *Answers) BaseURL() string {
	switch {
	case a.TLS():
		return "https://" + a.PanelHost()
	case a.HTTPPort != "80":
		return "http://" + a.PanelHost() + ":" + a.HTTPPort
	}
	return "http://" + a.PanelHost()
}

// Check normalises every answer in place and returns the first problem.
// busy reports a port something already listens on; nil skips that check.
func (a *Answers) Check(busy func(string) bool) error {
	var err error
	if a.DataDir, err = CheckDataDir(a.DataDir); err != nil {
		return fmt.Errorf("data dir: %w", err)
	}
	if a.Root, err = CheckRoot(a.Root); err != nil {
		return fmt.Errorf("domain: %w", err)
	}
	if a.Host != "" {
		if a.Host, err = CheckHost(a.Host, a.Root); err != nil {
			return fmt.Errorf("panel host: %w", err)
		}
	}
	if a.Proxies, err = CheckProxies(a.Proxies); err != nil {
		return fmt.Errorf("proxy: %w", err)
	}
	if a.TLS() {
		if a.Email, err = CheckEmail(a.Email); err != nil {
			return fmt.Errorf("email: %w", err)
		}
	} else {
		a.Email, a.HTTPSPort = "", ""
	}
	if err = CheckPort(a.HTTPPort, busy); err != nil {
		return fmt.Errorf("http port: %w", err)
	}
	if a.TLS() {
		if err = CheckHTTPSPort(a.HTTPSPort, a.HTTPPort, busy); err != nil {
			return fmt.Errorf("https port: %w", err)
		}
	}
	return nil
}

// CheckRoot accepts a domain name Let's Encrypt can issue for. The grammar
// lives in installspec so the panel's own domain form refuses the same hosts.
func CheckRoot(s string) (string, error) { return installspec.CheckRoot(s) }

// CheckHost accepts a name at or under root.
func CheckHost(s, root string) (string, error) {
	h, err := CheckRoot(s)
	if err != nil {
		return "", err
	}
	if h != root && !strings.HasSuffix(h, "."+root) {
		return "", fmt.Errorf("%s is not under %s", h, root)
	}
	return h, nil
}

// CheckProxies accepts addresses or ranges separated by commas or spaces,
// blank for none, and returns them comma joined without repeats.
func CheckProxies(s string) (string, error) {
	var out []string
	seen := map[string]bool{}
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		c, err := netaddr.ParseTrusted(f)
		if err != nil {
			return "", err
		}
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return strings.Join(out, ","), nil
}

// CheckEmail accepts an address Let's Encrypt can write to.
func CheckEmail(s string) (string, error) {
	s = strings.TrimSpace(s)
	local, domain, ok := strings.Cut(s, "@")
	switch {
	case s == "":
		return "", errors.New("an email address is required")
	case !ok:
		return "", fmt.Errorf("%s is not an email address", s)
	case strings.Contains(domain, "@"):
		return "", fmt.Errorf("%s has more than one @", s)
	case strings.ContainsAny(s, " \t"):
		return "", fmt.Errorf("%s has spaces", s)
	case local == "":
		return "", fmt.Errorf("%s has nothing before the @", s)
	}
	d, err := CheckRoot(domain)
	if err != nil || d != strings.ToLower(domain) {
		return "", fmt.Errorf("%s is not a domain Let's Encrypt can mail", domain)
	}
	return local + "@" + d, nil
}

// CheckPort accepts 1-65535; busy refuses a port already in use.
func CheckPort(s string, busy func(string) bool) error {
	n, err := strconv.Atoi(s)
	switch {
	case err != nil || s[0] < '1' || s[0] > '9':
		return fmt.Errorf("%q is not a port number", s)
	case n > 65535:
		return fmt.Errorf("%s is outside 1-65535", s)
	case busy != nil && busy(s):
		return fmt.Errorf("something is already listening on %s", s)
	}
	return nil
}

// CheckHTTPSPort is CheckPort that also refuses the HTTP port.
func CheckHTTPSPort(s, httpPort string, busy func(string) bool) error {
	if err := CheckPort(s, busy); err != nil {
		return err
	}
	if s == httpPort {
		return fmt.Errorf("HTTP already uses %s", s)
	}
	return nil
}

// CheckDataDir accepts an absolute path that can go into a --mount spec,
// where a comma would split it.
func CheckDataDir(s string) (string, error) {
	d := s
	for len(d) > 1 && strings.HasSuffix(d, "/") {
		d = strings.TrimSuffix(d, "/")
	}
	switch {
	case d == "/":
		return "", errors.New("not the filesystem root")
	case !filepath.IsAbs(d):
		return "", fmt.Errorf("%s needs to be an absolute path", d)
	case strings.Trim(d, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._/-") != "":
		return "", fmt.Errorf("%s has characters other than letters, digits, . _ - /", d)
	case filepath.Clean(d) != d:
		return "", fmt.Errorf("%s has . or .. or // in it; write the path out", d)
	}
	if fi, err := os.Stat(d); err == nil && !fi.IsDir() {
		return "", fmt.Errorf("%s exists and is not a directory", d)
	}
	return d, nil
}
