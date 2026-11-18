package promote

import (
	"reflect"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// A domain's proxy.forward_auth reaches the spec's extras.
func TestSpecOfForwardAuth(t *testing.T) {
	var dc DomainConf
	src := "host: a.io\nproxy:\n  forward_auth:\n    url: https://auth.a.io/check\n    copy_headers: [Remote-User]\n"
	if err := yaml.Unmarshal([]byte(src), &dc); err != nil {
		t.Fatal(err)
	}
	fa := specOf(dc, "a.io", 80).Extras.ForwardAuth
	if fa == nil || fa.URL != "https://auth.a.io/check" || !reflect.DeepEqual(fa.CopyHeaders, []string{"Remote-User"}) {
		t.Errorf("forward auth = %+v", fa)
	}
}

// network: host runs one replica and takes no published ports or lan lines.
func TestHostNetworkRefusals(t *testing.T) {
	for _, c := range []struct{ src, want string }{
		{"type: image\nimage: nginx:1\nnetwork: host\n", ""},
		{"type: image\nimage: nginx:1\nnetwork: bridge\n", "must be host"},
		{"type: image\nimage: nginx:1\nnetwork: host\nreplicas: 2\n", "one replica"},
		{"type: image\nimage: nginx:1\nnetwork: host\npublished_ports: [\"80:80\"]\n", "published_ports"},
		{"type: image\nimage: nginx:1\nnetwork: host\nlan: [all]\n", "drop lan"},
	} {
		var tc TileConf
		if err := yaml.Unmarshal([]byte(c.src), &tc); err != nil {
			t.Fatal(err)
		}
		err := checkTile("web", tc, "acme")
		if (c.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%q: err = %v, want %q", c.src, err, c.want)
		}
	}
}

func TestStackFileParamsPerEnv(t *testing.T) {
	head := "version: 1\nstack: s\nladder: [dev, prd]\nhead: main\n"
	r, err := Load([]byte(head+"params:\n  app:\n    dev|prd:\n      mode: fast\n    prd:\n      key: {type: secret, generate: 32}\n    pr:\n      mode: sandbox\nenvironments:\n  dev: {locked: false}\n  prd: {from: promote}\n"), nil, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if r.Params["dev"]["app.mode"].Value != "fast" || r.Params["prd"]["app.mode"].Value != "fast" ||
		!r.Params["prd"]["app.key"].Secret || r.Params["pr"]["app.mode"].Value != "sandbox" || len(r.Params["dev"]) != 1 {
		t.Errorf("params = %+v", r.Params)
	}
	if l := r.Envs["dev"].Locked; l == nil || *l || r.Envs["prd"].Locked != nil {
		t.Errorf("locked dev %v prd %v", r.Envs["dev"].Locked, r.Envs["prd"].Locked)
	}
	if _, err := Load([]byte("version: 1\nstack: s\nladder: [dev, pr]\nhead: main\n"), nil, "acme"); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Errorf("env named pr: err = %v", err)
	}
	for name, c := range map[string]struct{ src, want string }{
		"unknown env": {"params:\n  app:\n    demo:\n      a: x\n", `"demo" is not an environment of this stack`},
		"conflict":    {"params:\n  app:\n    dev|prd:\n      a: x\n    prd:\n      a: y\n", `by both "dev|prd" and "prd"`},
		"old shape":   {"params:\n  app:\n    mode: {type: param, value: x}\n", `"mode" is not an environment`},
	} {
		if _, err := Load([]byte(head+c.src), nil, "acme"); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

// Includes merge a group's env blocks name by name; an include wins, as it does for params today.
func TestStackFileParamsInclude(t *testing.T) {
	inc := "params:\n  app:\n    dev:\n      a: inc\n      b: inc\n    prd:\n      c: inc\n"
	main := "version: 1\nstack: s\nladder: [dev, prd]\nhead: main\ninclude: [x.yml]\nparams:\n  app:\n    dev:\n      a: main\nenvironments:\n  dev: {}\n  prd: {from: promote}\n"
	r, err := Load([]byte(main), func(string) ([]byte, error) { return []byte(inc), nil }, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if r.Params["dev"]["app.a"].Value != "inc" || r.Params["dev"]["app.b"].Value != "inc" || r.Params["prd"]["app.c"].Value != "inc" {
		t.Errorf("params = %+v", r.Params)
	}
}
