package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
)

func TestDevSettings(t *testing.T) {
	dir := t.TempDir()
	var cfg service.Config
	devSettings(slog.Default(), dir, &cfg) // no file: untouched
	if cfg.ACMECA != "" || cfg.ACMECARoot != "" || cfg.NamecheapAPI != "" || cfg.GitHubURL != "" || cfg.GitHubCA != "" {
		t.Fatalf("missing file changed config: %+v", cfg)
	}
	body := "# comment\n\nACME_CA=https://ca/dir\nACME_CA_ROOT=root.pem\nNAMECHEAP_API = http://nc/xml.response\nBOGUS=1\nNAMECHEAP_API=\nGITHUB_URL=https://gh:8443\nGITHUB_CA=/ca/gh.pem\n"
	if err := os.WriteFile(filepath.Join(dir, "dev.env"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	devSettings(slog.Default(), dir, &cfg)
	if cfg.ACMECA != "https://ca/dir" || cfg.ACMECARoot != "/run/stackr-admin/root.pem" || cfg.NamecheapAPI != "http://nc/xml.response" ||
		cfg.GitHubURL != "https://gh:8443" || cfg.GitHubCA != "/ca/gh.pem" {
		t.Errorf("got %+v", cfg)
	}
}
