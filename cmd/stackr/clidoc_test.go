package main

import (
	stdflag "flag"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

var updateDoc = stdflag.Bool("update", false, "rewrite docs/guide/cli.md from the command tree")

const cliDoc = "../../docs/guide/cli.md"

// cliReference renders the command tree: every command's usage, summary,
// flags and example, the same text --help prints.
func cliReference() string {
	root := (&app{}).root()
	host, _ := os.Hostname()
	var sb strings.Builder
	sb.WriteString("# CLI reference\n\n")
	sb.WriteString("Generated from the command definitions: `go test ./cmd/stackr -run TestCLIReference -update`. ")
	sb.WriteString("`stackr <command> --help` prints the same text.\n\n")
	sb.WriteString("```\n" + strings.TrimSpace(root.Long) + "\n```\n\n")
	sb.WriteString("## Flags on every command\n\n```\n" + strings.TrimRight(root.PersistentFlags().FlagUsages(), "\n ") + "\n```\n\n")
	sb.WriteString("Exit codes: 0 fine, 1 failed, 2 wrong command line, 3 refused without `--yes` (no terminal, or `--json`).\n\n")
	var walk func(c *cobra.Command, depth int)
	walk = func(c *cobra.Command, depth int) {
		if c.Hidden || c.Name() == "help" || c.Name() == "completion" {
			return
		}
		if depth > 0 {
			sb.WriteString(strings.Repeat("#", min(depth+1, 4)) + " `" + c.CommandPath() + "`\n\n")
			sb.WriteString(c.Short + "\n\n")
			if !c.HasSubCommands() || c.Runnable() && c.Long != "" {
				sb.WriteString("```\n" + c.UseLine() + "\n```\n\n")
			}
			if c.Long != "" {
				sb.WriteString(strings.TrimSpace(c.Long) + "\n\n")
			}
			if fl := strings.TrimRight(c.LocalNonPersistentFlags().FlagUsages(), "\n "); fl != "" && !c.HasSubCommands() {
				sb.WriteString("```\n" + fl + "\n```\n\n")
			}
			if pf := strings.TrimRight(c.PersistentFlags().FlagUsages(), "\n "); pf != "" {
				sb.WriteString("Flags for it and its subcommands:\n\n```\n" + pf + "\n```\n\n")
			}
			if c.Example != "" {
				sb.WriteString("Example:\n\n```\n" + strings.Trim(c.Example, "\n") + "\n```\n\n")
			}
		}
		for _, s := range c.Commands() {
			walk(s, depth+1)
		}
	}
	walk(root, 0)
	return strings.ReplaceAll(sb.String(), "cli@"+host, "cli@<hostname>")
}

// docs/guide/cli.md is the command tree, not a hand copy of it.
func TestCLIReference(t *testing.T) {
	want := cliReference()
	if *updateDoc {
		if err := os.WriteFile(cliDoc, []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile(cliDoc)
	if err != nil || string(got) != want {
		t.Fatal(fmt.Sprintf("%s is stale: go test ./cmd/stackr -run TestCLIReference -update", cliDoc))
	}
}
