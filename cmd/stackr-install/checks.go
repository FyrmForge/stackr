package main

import (
	"errors"
	"fmt"
	"net/netip"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/FyrmForge/stackr/internal/installspec"
)

// check normalises every answer in place and returns the first problem.
// busy reports a port something already listens on; nil skips that check.
func check(in *installspec.Input, busy func(string) bool) error {
	var err error
	if in.DataDir, err = checkDataDir(in.DataDir); err != nil {
		return fmt.Errorf("data dir: %w", err)
	}
	if in.Root, err = installspec.CheckRoot(in.Root); err != nil {
		return fmt.Errorf("domain: %w", err)
	}
	if in.PanelHost == "" {
		in.PanelHost = "stkr." + strings.TrimPrefix(in.Root, "*.")
	} else if in.PanelHost, err = checkHost(in.PanelHost, in.Root); err != nil {
		return fmt.Errorf("panel host: %w", err)
	}
	if in.Proxies, err = checkProxies(in.Proxies); err != nil {
		return fmt.Errorf("proxy: %w", err)
	}
	if in.HTTPS {
		if in.Email, err = checkEmail(in.Email); err != nil {
			return fmt.Errorf("email: %w", err)
		}
	} else {
		in.Email, in.HTTPSPort, in.DNS01 = "", "", false
	}
	if err = checkPort(in.HTTPPort, busy); err != nil {
		return fmt.Errorf("http port: %w", err)
	}
	if in.HTTPS {
		if err = checkPort(in.HTTPSPort, busy); err != nil {
			return fmt.Errorf("https port: %w", err)
		}
		if in.HTTPSPort == in.HTTPPort {
			return errors.New("https port: the same as the http port")
		}
	}
	return nil
}

// checkHost accepts a name at or under root.
func checkHost(s, root string) (string, error) {
	h, err := installspec.CheckRoot(s)
	if err != nil {
		return "", err
	}
	base := strings.TrimPrefix(root, "*.")
	if h != base && !strings.HasSuffix(h, "."+base) {
		return "", fmt.Errorf("%s is not under %s", h, base)
	}
	return h, nil
}

// checkProxies: addresses or ranges split on commas or spaces, canonical,
// de-duplicated, comma-joined. A bare address is a single-host range; /0
// would trust every address on the internet to name the client.
func checkProxies(s string) (string, error) {
	var out []string
	seen := map[string]bool{}
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		p, err := netip.ParsePrefix(f)
		if err != nil {
			a, aerr := netip.ParseAddr(f)
			if aerr != nil {
				return "", fmt.Errorf("%q is not an IP address or range", f)
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		if p.Bits() == 0 {
			return "", fmt.Errorf("%q trusts every address; name your proxy's address instead", f)
		}
		c := p.Masked().String()
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return strings.Join(out, ","), nil
}

// checkEmail: one @, no spaces, something before it, a real domain after.
func checkEmail(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	local, dom, ok := strings.Cut(s, "@")
	if !ok || local == "" || strings.Contains(dom, "@") || strings.ContainsAny(s, " \t") {
		return "", fmt.Errorf("%q is not an email address", s)
	}
	if _, err := installspec.CheckRoot(dom); err != nil || strings.HasPrefix(dom, "*.") {
		return "", fmt.Errorf("%q does not end in a real domain", s)
	}
	return s, nil
}

// checkPort: 1-65535, no leading zero, and not already taken.
func checkPort(s string, busy func(string) bool) error {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 65535 || s[0] == '0' {
		return fmt.Errorf("%q is not a port", s)
	}
	if busy != nil && busy(s) {
		return fmt.Errorf("something already listens on %s", s)
	}
	return nil
}

var dataDirChars = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// checkDataDir: absolute, cleaned, not /. The character rule keeps the path
// safe inside a -v spec, where a colon or comma would split it.
func checkDataDir(s string) (string, error) {
	if !filepath.IsAbs(s) {
		return "", fmt.Errorf("%q is not an absolute path", s)
	}
	s = filepath.Clean(s)
	if s == "/" {
		return "", errors.New("not the root directory")
	}
	if !dataDirChars.MatchString(s) {
		return "", fmt.Errorf("%q may hold letters, digits and ._-/ only", s)
	}
	return s, nil
}

var versionRe = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)

// checkVersion: what reaches an image tag. "latest" or "" is this binary's
// own release; a dev binary has none.
func checkVersion(s, own string) (string, error) {
	if s == "" || s == "latest" {
		s = own
	}
	s = strings.TrimPrefix(s, "v")
	if !versionRe.MatchString(s) {
		return "", fmt.Errorf("%q is not a release version like 0.1.0 (a dev build needs --version)", s)
	}
	return s, nil
}
