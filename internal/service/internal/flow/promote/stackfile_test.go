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
