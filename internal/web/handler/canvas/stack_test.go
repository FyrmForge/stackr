package canvas_test

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// A refused save keeps what was typed and says why on the field, in plain
// words: the Resources form (a bad CPU beside a good memory), a rename (in
// its General row, opened, the summary still the stored name) and a
// variables add (its form open again, never a secret echoed).
func TestRefusedSaveKeepsTyped(t *testing.T) {
	s := webtest.New(t)
	const d = "/acme/shop/-/drawer"
	rec := s.Do(t, "POST", d+"/settings", url.Values{"cpu_limit": {"lots"}, "mem_limit_mb": {"300"}, "protect_password": {"hunter2"}})
	body := rec.Body.String()
	for _, w := range []string{`value="lots"`, `value="300"`, `id="error-cpu_limit" data-has-error="true" aria-live="polite">&#34;lots&#34; is not a number.`} {
		if rec.Code != 422 || !strings.Contains(body, w) {
			t.Errorf("resources = %d, lacks %s\n%s", rec.Code, w, body)
		}
	}
	if strings.Contains(body, "hunter2") {
		t.Error("the refused form echoes the password")
	}

	rec = s.Do(t, "POST", d+"/rename", url.Values{"open": {"general"}, "name": {"  "}})
	body = rec.Body.String()
	_, general, _ := strings.Cut(body, `id="stack-settings-general"`)
	general, _, _ = strings.Cut(general, "</details>")
	if rec.Code != 422 || !strings.Contains(general, `data-acc="general" open`) || !strings.Contains(general, `data-acc-sum>shop<`) ||
		!strings.Contains(general, `data-has-error="true"`) || strings.Contains(body, "name:") {
		t.Errorf("rename = %d\n%s", rec.Code, general)
	}

	rec = s.Do(t, "POST", "/acme/shop/-/vars", url.Values{"add_form": {""}, "add_group": {"app"}, "add_name": {"GREETING"}, "add_value.stack_pr": {"hi"}, "add_secret.stack_pr": {"s3cret"}})
	body = rec.Body.String()
	for _, w := range []string{`<details class="group" open>`, `value="app"`, `value="GREETING"`, `value="hi"`, "Use lower-case letters, digits and _ in a name, like greeting."} {
		if rec.Code != 422 || !strings.Contains(body, w) {
			t.Errorf("vars add = %d, lacks %s\n%s", rec.Code, w, body)
		}
	}
	if strings.Contains(body, "s3cret") || strings.Count(body, " open>") != 1 || strings.Contains(body, "name:") {
		t.Errorf("vars add echoes a secret, opens more than its form, or shows the key\n%s", body)
	}
}

// An Add row marks its group and name each at once, refuses a blank one
// instead of "Saved.", and a saved add's note sits right after its form.
func TestVarsAddAnswers(t *testing.T) {
	s := webtest.New(t)
	rec := s.Do(t, "POST", "/acme/shop/-/vars", url.Values{"add_form": {""}, "add_group": {"bad g!"}, "add_name": {"bad n!"}, "add_value.stack_pr": {"v"}})
	body := rec.Body.String()
	if rec.Code != 422 || !strings.Contains(body, `aria-describedby="error-add_group"`) || !strings.Contains(body, `aria-describedby="error-add_name"`) {
		t.Errorf("bad group and name = %d\n%s", rec.Code, body)
	}
	if rec := s.Do(t, "POST", "/acme/shop/-/vars", url.Values{"add_form": {""}}); rec.Code != 422 || !strings.Contains(rec.Body.String(), "Give the variable a name.") {
		t.Errorf("empty add = %d\n%s", rec.Code, rec.Body)
	}
	rec = s.Do(t, "POST", "/acme/shop/-/vars", url.Values{"add_form": {""}, "add_group": {"app"}, "add_name": {"greeting"}, "add_value.stack_pr": {"hi"}})
	body = rec.Body.String()
	form, note := strings.LastIndex(body, "+ Add group"), strings.Index(body, "data-acted")
	if rec.Code != 200 || note < form || !strings.Contains(body[note:], "Saved.") {
		t.Errorf("saved add = %d, note at %d, add form at %d\n%s", rec.Code, note, form, body)
	}
}

// The stack's Settings tab is one accordion holding what were the Params
// and Placement tabs; Env order moves the ladder from here.
func TestStackSettingsRows(t *testing.T) {
	s := webtest.New(t)
	st := stagingOf(t, s)
	const d = "/acme/shop/-/drawer"
	body := get(t, s, d+"?tab=settings&open=order")
	for _, w := range []string{
		`data-acc="order" open`, "dev, staging",
		`data-acc="variables"`, `id="vars-editor"`,
		`data-acc="placement"`, `hx-get="/acme/shop/-/placement"`, "inherits org (default)",
		`data-acc="config"`, "managed from the UI",
		`data-acc="resources"`, "inherits org",
		`data-acc="host"`, `data-acc="delete"`,
		`hx-post="` + d + `/order"`,
	} {
		if !strings.Contains(body, w) {
			t.Errorf("no %s in\n%s", w, body)
		}
	}
	for _, gone := range []string{"tab=params", "tab=placement"} {
		if strings.Contains(body, gone) {
			t.Errorf("the drawer still offers %s", gone)
		}
	}
	rec := s.Do(t, "POST", d+"/order", url.Values{"ids": {st.ID, s.Tile.Env}})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "staging, dev") {
		t.Errorf("reorder = %d\n%s", rec.Code, rec.Body)
	}
	es, err := s.Orch.Ladder(context.Background(), s.Tile.Stack)
	if err != nil || len(es) != 2 || es[0].ID != st.ID {
		t.Errorf("ladder after the move = %v, %v", es, err)
	}
}
