package installer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"golang.org/x/term"

	"github.com/FyrmForge/stackr/internal/installspec"
)

// keys adds the arrow keys to moving between fields; huh only has tab and
// enter. Left and right stay with the text cursor and the toggles.
func keys() *huh.KeyMap {
	k := huh.NewDefaultKeyMap()
	k.Input.Prev.SetKeys("shift+tab", "up")
	k.Input.Next.SetKeys("enter", "tab", "down")
	k.Confirm.Prev.SetKeys("shift+tab", "up")
	k.Confirm.Next.SetKeys("enter", "tab", "down")
	// Three choices on the summary need no search.
	k.Select.Filter.SetEnabled(false)
	return k
}

// ask fills a from the form, one question per screen with the answers so far
// listed above it. One field per group: huh puts a group's errors in one
// footer under the last field, so a page of fields shows the error for the
// first one a long way from it.
func ask(a *Answers, dry bool) error {
	busy := portBusy
	if dry {
		// A dry run is usually not on the box that gets the install.
		busy = nil
	}
	yes := func(title string, v *bool) *huh.Confirm {
		return huh.NewConfirm().Title(title).Affirmative("Yes").Negative("No").Value(v)
	}
	// step wraps one question with the recap of the questions before it.
	step := func(n int, field huh.Field) *huh.Group {
		if n == 0 {
			return huh.NewGroup(field)
		}
		return huh.NewGroup(
			huh.NewNote().DescriptionFunc(func() string { return recap(a, n) }, a),
			field,
		)
	}
	form := huh.NewForm(
		step(0, huh.NewInput().Title("Root domain").Placeholder("example.com").Value(&a.Root).
			Description("Apps get names under it: app.stack.org.example.com.").
			Validate(func(s string) error { _, err := CheckRoot(s); return err })),
		step(1, huh.NewInput().Title("Panel hostname").Value(&a.Host).
			PlaceholderFunc(func() string { return "stkr." + installspec.CleanHost(a.Root) }, &a.Root).
			Description("Blank for the one shown.").
			Validate(func(s string) error {
				if strings.TrimSpace(s) == "" {
					return nil
				}
				_, err := CheckHost(s, installspec.CleanHost(a.Root))
				return err
			})),
		step(2, yes("Behind Cloudflare?", &a.Cloudflare)),
		step(3, huh.NewInput().Title("Your proxy's IP address or range").Value(&a.Proxies).
			Description("Comma separated. Blank for none.").
			Validate(func(s string) error { _, err := CheckProxies(s); return err })),
		step(4, yes("Turn on HTTPS?", &a.HTTPS).
			Description("Let's Encrypt must reach the panel hostname from the internet.")),
		step(5, huh.NewInput().Title("HTTP port").Value(&a.HTTPPort).
			Validate(func(s string) error { return CheckPort(s, busy) })),
		step(6, huh.NewInput().Title("Email for Let's Encrypt").Value(&a.Email).
			Validate(func(s string) error { _, err := CheckEmail(s); return err })).
			WithHideFunc(func() bool { return !a.TLS() }),
		step(7, huh.NewInput().Title("HTTPS port").Value(&a.HTTPSPort).
			Validate(func(s string) error { return CheckHTTPSPort(s, a.HTTPPort, busy) })).
			WithHideFunc(func() bool { return !a.TLS() }),
	).WithKeyMap(keys()).WithHeight(formHeight())
	return form.Run()
}

// formHeight keeps every screen the same size. Left to itself huh measures
// the groups before the recaps have rendered, sizes them all to the shortest,
// and the earliest answers then scroll out of view.
func formHeight() int {
	const want = 16 // 8 recap lines, the question, its error, the help line
	if _, h, err := term.GetSize(int(os.Stdout.Fd())); err == nil && h > 0 && h < want {
		return h
	}
	return want
}

// recap is the answers to the first n questions, in the order they are asked.
func recap(a *Answers, n int) string {
	rows := [][2]string{
		{"root domain", a.Root},
		{"panel", a.PanelHost()},
		{"cloudflare", onOff(a.Cloudflare)},
		{"proxy", or(a.Proxies, "none")},
		{"https", onOff(a.HTTPS)},
		{"http port", a.HTTPPort},
		{"email", a.Email},
		{"https port", a.HTTPSPort},
	}
	if n > len(rows) {
		n = len(rows)
	}
	var b strings.Builder
	dim := lipgloss.NewStyle().Faint(true)
	for _, r := range rows[:n] {
		_, _ = fmt.Fprintf(&b, "%-14s %s\n", r[0], r[1])
	}
	return dim.Render(strings.TrimRight(b.String(), "\n"))
}

func onOff(v bool) string {
	if v {
		return "on"
	}
	return "off"
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// Choice is what the summary screen returns.
type Choice string

const (
	ChoiceInstall Choice = "install"
	ChoiceBack    Choice = "back"
	ChoiceCancel  Choice = "cancel"
)

// confirm shows what will be installed, with any DNS warnings, and asks.
func confirm(a *Answers, warnings []string, dry bool) (Choice, error) {
	pick := ChoiceInstall
	label := "Install"
	if dry {
		label = "Show what would run"
	}
	// A note renders * _ ` as markup, and *.example.com and my_data are
	// answers.
	plain := strings.NewReplacer(`\`, `\\`, "*", `\*`, "_", `\_`, "`", "\\`")
	desc := plain.Replace(summary(a))
	if len(warnings) > 0 {
		warn := lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
		desc += "\n\n" + warn.Render(plain.Replace(strings.Join(warnings, "\n")))
	}
	err := huh.NewForm(huh.NewGroup(
		huh.NewNote().Title("Ready to install").Description(desc),
		huh.NewSelect[Choice]().Value(&pick).Options(
			huh.NewOption(label, ChoiceInstall),
			huh.NewOption("Go back", ChoiceBack),
			huh.NewOption("Cancel", ChoiceCancel),
		),
	)).WithKeyMap(keys()).Run()
	return pick, err
}

func summary(a *Answers) string {
	var b strings.Builder
	row := func(k, v string) {
		if v != "" {
			_, _ = fmt.Fprintf(&b, "%-16s %s\n", k, v)
		}
	}
	row("panel", a.BaseURL())
	row("apps under", a.Root)
	row("cloudflare", onOff(a.Cloudflare))
	row("proxy", a.Proxies)
	row("https", onOff(a.TLS()))
	row("email", a.Email)
	row("http port", a.HTTPPort)
	row("https port", a.HTTPSPort)
	row("data", a.DataDir)
	return strings.TrimRight(b.String(), "\n")
}

// dnsWarnings looks up the panel host and a made-up name under the root, the
// two records Let's Encrypt needs. Only whether they resolve: behind
// Cloudflare or a proxy they never point at this box, so that is not checked.
func dnsWarnings(ctx context.Context, a *Answers, lookup func(context.Context, string) ([]string, error)) []string {
	if !a.TLS() {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var out []string
	if _, err := lookup(ctx, a.PanelHost()); err != nil {
		out = append(out, fmt.Sprintf("DNS: %s does not resolve yet. Add an A record for it.", a.PanelHost()))
	}
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	if _, err := lookup(ctx, "stackr-check-"+hex.EncodeToString(b)+"."+a.Root); err != nil {
		out = append(out, fmt.Sprintf("DNS: *.%s does not resolve yet. Add a wildcard A record.", a.Root))
	}
	if len(out) > 0 {
		out = append(out, "Certificates fail until both resolve. You can install now and add them after.")
	}
	return out
}

var lookupHost = net.DefaultResolver.LookupHost
