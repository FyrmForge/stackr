package promote

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domain"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// createDetails is what a create row shows when opened: what the new tile
// will be, from the file's block and the row it makes, one "Label: value"
// per line. A line with no value is left out.
//
// node is the node the env runs on, "" = not known yet.
//
// vol, when set, words each volume spec of the block ("pgdata, new, 5120 MB").
func createDetails(tc TileConf, row store.Tile, node string, vol func(spec string) string) string {
	var out []string
	add := func(label, v string) {
		if v != "" {
			out = append(out, label+": "+v)
		}
	}
	add("Engine", tc.Engine)
	if tc.Major > 0 {
		add("Major", strconv.Itoa(tc.Major))
	}
	add("Image", row.ImageRef)
	add("Node", node)
	add("Entrypoint", string(tc.Entrypoint))
	add("Command", tc.Command)
	if tc.Replicas > 1 {
		add("Replicas", strconv.Itoa(tc.Replicas))
	}
	specs := tc.Volumes
	if vol != nil {
		specs = nil
		for _, s := range tc.Volumes {
			specs = append(specs, vol(s))
		}
	}
	add("Volumes", strings.Join(specs, ", "))
	return strings.Join(out, "\n")
}

// domainDiff compares a stored domain row with the spec the file now asks
// for, field by field: details is one "Label: change" per line, note the
// short list ("basic_auth removed, forward_auth added"). Never a password
// or a header value; "" for both = nothing the compare knows.
func domainDiff(row store.Domain, sp domain.Spec) (details, note string) {
	var lines, notes []string
	// say records a change: line is the opened form, short the note's word.
	say := func(line, short string) {
		lines = append(lines, line)
		notes = append(notes, short)
	}
	val := func(label, a, b string) {
		switch {
		case a == b:
		case a == "":
			say(label+": added "+b, label+" added")
		case b == "":
			say(label+": removed", label+" removed")
		default:
			say(label+": "+a+" to "+b, label+" changed")
		}
	}
	onOff := func(b bool) string { return map[bool]string{true: "on", false: "off"}[b] }
	flag := func(label string, a, b bool) {
		if a != b {
			say(label+": "+onOff(a)+" to "+onOff(b), label+" "+onOff(b))
		}
	}

	if row.ContainerPort != sp.Port {
		say("port: "+strconv.Itoa(row.ContainerPort)+" to "+strconv.Itoa(sp.Port), "port changed")
	}
	flag("https", row.HTTPS, domain.On(sp.HTTPS))
	flag("force_https", row.ForceHTTPS, domain.On(sp.ForceHTTPS))
	val("redirect_to", row.RedirectTo, strings.ToLower(sp.RedirectTo))
	if row.Auto != sp.Auto {
		say("auto: "+onOff(row.Auto)+" to "+onOff(sp.Auto), "auto changed")
	}
	if deref(row.ResourceID) != deref(sp.ResourceID) {
		say("domain resource: changed", "domain resource changed")
	}
	if sigRecords(row.Records) != sigRecords(sp.Records) {
		say("records: changed", "records changed")
	}

	var o, n domain.Extras
	_ = json.Unmarshal([]byte(row.ProxyJSON), &o) // a bad row reads as no extras
	n = sp.Extras
	switch {
	case o.BasicAuth == nil && n.BasicAuth != nil:
		say("basic_auth: added", "basic_auth added")
	case o.BasicAuth != nil && n.BasicAuth == nil:
		say("basic_auth: removed", "basic_auth removed")
	case o.BasicAuth != nil && *o.BasicAuth != *n.BasicAuth:
		if o.BasicAuth.User != n.BasicAuth.User {
			say("basic_auth user: "+o.BasicAuth.User+" to "+n.BasicAuth.User, "basic_auth changed")
		}
		if o.BasicAuth.Password != n.BasicAuth.Password {
			say("basic_auth password: changed", "basic_auth changed")
		}
	}
	switch {
	case o.ForwardAuth == nil && n.ForwardAuth != nil:
		say("forward_auth url: added "+n.ForwardAuth.URL, "forward_auth added")
	case o.ForwardAuth != nil && n.ForwardAuth == nil:
		say("forward_auth url: removed", "forward_auth removed")
	case o.ForwardAuth != nil && !reflect.DeepEqual(o.ForwardAuth, n.ForwardAuth):
		if o.ForwardAuth.URL != n.ForwardAuth.URL {
			say("forward_auth url: "+o.ForwardAuth.URL+" to "+n.ForwardAuth.URL, "forward_auth changed")
		}
		if !slices.Equal(o.ForwardAuth.CopyHeaders, n.ForwardAuth.CopyHeaders) {
			say("forward_auth copy_headers: changed", "forward_auth changed")
		}
	}
	flag("websockets", o.Websockets, n.Websockets)
	flag("strip_prefix", o.StripPrefix, n.StripPrefix)
	flag("sec_headers", o.SecHeaders, n.SecHeaders)
	if o.MaxBodyMB != n.MaxBodyMB {
		say("max_body_mb: "+strconv.Itoa(o.MaxBodyMB)+" to "+strconv.Itoa(n.MaxBodyMB), "max_body_mb changed")
	}
	if !reflect.DeepEqual(o.Timeouts, n.Timeouts) {
		say("timeouts: changed", "timeouts changed")
	}
	if !slices.Equal(o.Methods, n.Methods) {
		say("methods: "+strings.Join(o.Methods, ",")+" to "+strings.Join(n.Methods, ","), "methods changed")
	}
	if !reflect.DeepEqual(o.Headers, n.Headers) { // names only: a value may be a token
		say("headers: changed", "headers changed")
	}
	return strings.Join(lines, "\n"), strings.Join(notes, ", ")
}

// resDiff is domainDiff for a stack-level domain resource: its two settings.
func resDiff(old, now store.DomainResource) (details, note string) {
	var lines, notes []string
	if old.IncludeEnvOnDefault != now.IncludeEnvOnDefault {
		lines = append(lines, fmt.Sprintf("include_env_on_default: %t to %t", old.IncludeEnvOnDefault, now.IncludeEnvOnDefault))
		notes = append(notes, "include_env_on_default changed")
	}
	if old.ACMEEmail != now.ACMEEmail {
		lines = append(lines, "acme_email: "+orNone(old.ACMEEmail)+" to "+orNone(now.ACMEEmail))
		notes = append(notes, "acme_email changed")
	}
	return strings.Join(lines, "\n"), strings.Join(notes, ", ")
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
