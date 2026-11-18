package main

import (
	"bufio"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service"
)

// devSettings reads <dataDir>/dev.env once at start: KEY=VALUE lines, #
// comments and blank lines ignored. A missing file changes nothing. It is
// the playground's way to point stackrd at a private ACME CA, a fake
// Namecheap and a fake GitHub; production has no such file.
func devSettings(log *slog.Logger, dataDir string, cfg *service.Config) {
	f, err := os.Open(filepath.Join(dataDir, "dev.env"))
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Warn("dev.env unreadable", "err", err)
		}
		return
	}
	defer func() { _ = f.Close() }()
	var active []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, _ := strings.Cut(line, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if v == "" {
			continue
		}
		switch k {
		case "ACME_CA":
			cfg.ACMECA = v
		case "ACME_CA_ROOT": // a file name in the proxy admin dir, same on every node
			cfg.ACMECARoot = path.Join(installspec.ProxyAdminDir, v)
		case "NAMECHEAP_API":
			cfg.NamecheapAPI = v
		case "GITHUB_URL":
			cfg.GitHubURL = v
		case "GITHUB_CA": // a full path inside the panel container
			cfg.GitHubCA = v
		default:
			log.Warn("dev.env: unknown key ignored", "key", k)
			continue
		}
		active = append(active, k)
	}
	if len(active) > 0 {
		log.Info("dev.env active", "keys", strings.Join(active, ","))
	}
}
