package promote

import (
	"reflect"
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
