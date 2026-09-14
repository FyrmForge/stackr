package nodes

import "testing"

// A public address means the swarm ports and a plain-VXLAN data plane are
// exposed to the internet. The check is the only thing standing between an
// operator pasting a VPS's public IP and that.
func TestValidateAddress(t *testing.T) {
	ok := []string{"10.0.0.20", "10.0.0.4", "172.16.5.9", "100.64.1.2", "127.0.0.1"}
	for _, a := range ok {
		if err := ValidateAddress(a); err != nil {
			t.Fatalf("%s should be accepted: %v", a, err)
		}
	}
	bad := []string{"1.1.1.1", "203.0.113.7", "172.32.0.1", "worker.example.com", ""}
	for _, a := range bad {
		if err := ValidateAddress(a); err == nil {
			t.Fatalf("%q should be refused", a)
		}
	}
}
