package promote

import (
	"strings"
	"testing"
)

// A stack file may not ref server params: orgs never read server secrets, so
// the file is refused at load, not at the first deploy.
func TestStackFileRefusesServerRef(t *testing.T) {
	file := strings.Replace(sliceShopFile, "      image: ghcr.io/acme/api:1.4\n      env:\n",
		"      image: ghcr.io/acme/api:1.4\n      env:\n        KEY: ${{ server.params.s3.secret_key }}\n", 1)
	_, err := Load([]byte(file), nil, "testorg")
	if err == nil || !strings.Contains(err.Error(), "server params are readable only in stackr-server.yml") {
		t.Fatalf("load = %v, want the server ref refused", err)
	}
	if _, err := Load([]byte(sliceShopFile), nil, "testorg"); err != nil {
		t.Fatalf("the file without it = %v", err)
	}
}
