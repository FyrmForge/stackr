package main

import (
	"errors"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/FyrmForge/stackr/internal/installspec"
)

// The questions read like `gh pr create`: one at a time, inline, each answer
// left behind as a "? Question  answer" line. One program runs them all, so
// nothing typed between two questions is lost.

var (
	pGreen = lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Bold(true)
	pCyan  = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	pBold  = lipgloss.NewStyle().Bold(true)
	pFaint = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	pRed   = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
)

type askKind int

const (
	askText askKind = iota
	askSecret
	askYesNo // out gets "Yes" or "No"
	askPick  // out gets one of options
)

// question is one prompt. check cleans the answer or says how to fix it;
// the cleaned value is what out gets and what the answered line shows.
type question struct {
	title   string
	kind    askKind
	def     func() string // shown faint, taken on an empty Enter; "y"/"n" for askYesNo
	skip    func() bool   // asked at its turn, so it sees earlier answers
	check   func(string) (string, error)
	options []string
	before  func() string // printed above the question when it comes up
	pad     int           // title width, so a pair's answers line up
	out     *string
}

var errCancelled = errors.New("cancelled")

// ask runs the questions in order. Ctrl+C or Esc cancels.
func ask(qs []question) error {
	in := textinput.New()
	in.Prompt = ""
	in.SetWidth(60)
	in.Focus()
	m := asker{qs: qs, in: in}
	if m.next(); m.i == len(qs) {
		return nil // every question skipped
	}
	final, err := tea.NewProgram(m).Run()
	if err != nil {
		return err
	}
	if final.(asker).cancelled {
		return errCancelled
	}
	return nil
}

type asker struct {
	qs        []question
	i         int
	in        textinput.Model
	pick      int
	err       string
	cancelled bool
}

func prefix(title string, pad int) string {
	return pGreen.Render("?") + " " + pBold.Render(title) + strings.Repeat(" ", max(0, pad-len(title))) + "  "
}

// next moves to the first question still to ask.
func (m *asker) next() tea.Cmd {
	for m.i < len(m.qs) && m.qs[m.i].skip != nil && m.qs[m.i].skip() {
		m.i++
	}
	if m.i == len(m.qs) {
		return tea.Quit
	}
	q := m.qs[m.i]
	m.in.Reset()
	m.in.EchoMode = textinput.EchoNormal
	if q.kind == askSecret {
		m.in.EchoMode = textinput.EchoPassword
	}
	m.in.Placeholder = ""
	if q.def != nil && q.kind == askText {
		m.in.Placeholder = q.def()
	}
	m.pick, m.err = 0, ""
	if q.before != nil {
		return tea.Println(q.before())
	}
	return nil
}

func (m asker) Init() tea.Cmd {
	if m.i < len(m.qs) && m.qs[m.i].before != nil {
		return tea.Println(m.qs[m.i].before())
	}
	return nil
}

func (m asker) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	k, ok := msg.(tea.KeyPressMsg)
	if !ok || m.i >= len(m.qs) {
		var cmd tea.Cmd
		m.in, cmd = m.in.Update(msg)
		return m, cmd
	}
	q := m.qs[m.i]
	switch k.String() {
	case "ctrl+c", "esc":
		m.cancelled = true
		return m, tea.Quit
	case "up", "down":
		if q.kind == askPick {
			if k.String() == "up" && m.pick > 0 {
				m.pick--
			} else if k.String() == "down" && m.pick < len(q.options)-1 {
				m.pick++
			}
			return m, nil
		}
	case "enter":
		v, err := answer(q, m.in.Value(), m.pick)
		if err != nil {
			m.err = err.Error()
			return m, nil
		}
		*q.out = v
		shown := v
		if q.kind == askSecret {
			shown = strings.Repeat("*", len([]rune(v)))
		}
		line := prefix(q.title, q.pad) + pCyan.Render(shown)
		m.i++
		cmd := m.next()
		return m, tea.Sequence(tea.Println(line), cmd)
	}
	if q.kind == askPick {
		return m, nil
	}
	var cmd tea.Cmd
	m.in, cmd = m.in.Update(msg)
	return m, cmd
}

// answer turns what was typed (or picked) into the question's value.
func answer(q question, typed string, pick int) (string, error) {
	v := typed
	if q.kind != askSecret {
		v = strings.TrimSpace(v)
	}
	if v == "" && q.def != nil {
		v = q.def()
	}
	switch q.kind {
	case askPick:
		v = q.options[pick]
	case askYesNo:
		switch strings.ToLower(v) {
		case "y", "yes":
			v = "Yes"
		case "n", "no":
			v = "No"
		default:
			return "", errors.New("type y or n")
		}
	}
	if q.check != nil {
		return q.check(v)
	}
	return v, nil
}

func (m asker) View() tea.View {
	if m.cancelled || m.i >= len(m.qs) {
		return tea.NewView("")
	}
	q := m.qs[m.i]
	var b strings.Builder
	switch q.kind {
	case askPick:
		b.WriteString(prefix(q.title, q.pad) + pFaint.Render("[Use arrows to move, enter to pick]"))
		for j, o := range q.options {
			if j == m.pick {
				b.WriteString("\n" + pCyan.Render("> "+o))
			} else {
				b.WriteString("\n  " + o)
			}
		}
	case askYesNo:
		hint := "(Y/n) "
		if q.def != nil && q.def() == "n" {
			hint = "(y/N) "
		}
		b.WriteString(prefix(q.title, q.pad) + pFaint.Render(hint) + m.in.View())
	default:
		b.WriteString(prefix(q.title, q.pad) + m.in.View())
	}
	if m.err != "" {
		b.WriteString("\n" + pRed.Render("X "+m.err))
	}
	return tea.NewView(b.String())
}

// answers is what the questions fill: strings, turned into the install's
// Input by apply. given names the flags passed, which are not asked.
type answers struct {
	root, panel, https, email, wildcard, dnsToken     string
	cdn, proxies, adminEmail, password, confirm, next string
}

const (
	nextInstall = "Install"
	nextDryRun  = "Dry run: show what would change"
	nextCancel  = "Cancel"
)

// installQuestions asks for every answer not given as a flag. withAdmin
// adds the admin's email and password; summary prints before the last pick.
func installQuestions(a *answers, given map[string]bool, withAdmin, adminEmailGiven, passwordGiven bool,
	summary func() string) []question {
	yes := func() string { return "y" }
	no := func() string { return "n" }
	not := func(flag string) func() bool { return func() bool { return given[flag] } }
	pw := len("Confirm password")
	qs := []question{
		{title: "Root domain", out: &a.root, skip: not("domain"), check: installspec.CheckRoot},
		{title: "Panel address", out: &a.panel, skip: not("panel-host"),
			def:   func() string { return "stkr." + strings.TrimPrefix(a.root, "*.") },
			check: func(s string) (string, error) { return checkHost(s, a.root) }},
		{title: "Serve HTTPS with automatic certificates?", kind: askYesNo, def: yes, out: &a.https, skip: not("https")},
		{title: "Email for Let's Encrypt", out: &a.email, check: checkEmail,
			skip: func() bool { return given["email"] || a.https == "No" }},
		{title: "Wildcard certificates through Cloudflare DNS?", kind: askYesNo, def: no, out: &a.wildcard,
			skip: func() bool { return given["dns-token"] || a.https == "No" }},
		{title: "Cloudflare API token", kind: askSecret, out: &a.dnsToken, check: required("paste the token"),
			skip: func() bool { return given["dns-token"] || a.wildcard != "Yes" }},
		{title: "Behind a CDN or load balancer?", kind: askYesNo, def: no, out: &a.cdn, skip: not("proxy")},
		{title: "Its IP addresses or ranges", out: &a.proxies,
			skip: func() bool { return given["proxy"] || a.cdn != "Yes" },
			check: func(s string) (string, error) {
				if s == "" {
					return "", errors.New("enter at least one, like 10.0.0.0/8")
				}
				return checkProxies(s)
			}},
	}
	if withAdmin {
		qs = append(qs,
			question{title: "Admin email", out: &a.adminEmail, check: checkEmail,
				skip: func() bool { return adminEmailGiven }},
			question{title: "Admin password", pad: pw, kind: askSecret, out: &a.password, check: checkPassword,
				skip: func() bool { return passwordGiven }},
			question{title: "Confirm password", pad: pw, kind: askSecret, out: &a.confirm,
				skip: func() bool { return passwordGiven },
				check: func(s string) (string, error) {
					if s != a.password {
						return "", errors.New("the two passwords do not match; type it again")
					}
					return s, nil
				}},
		)
	}
	return append(qs, question{title: "What's next?", kind: askPick, out: &a.next,
		options: []string{nextInstall, nextDryRun, nextCancel}, before: summary})
}

func required(fix string) func(string) (string, error) {
	return func(s string) (string, error) {
		if s == "" {
			return "", errors.New(fix)
		}
		return s, nil
	}
}
