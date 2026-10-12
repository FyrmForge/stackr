package vip

import (
	"net/netip"
	"strings"
	"testing"
)

// A node in a cluster lets wg0 traffic past Docker's raw drop, keeps env
// sources into the mesh, and accepts only the listed flows on the pre-NAT VIP.
func TestRenderMeshFlows(t *testing.T) {
	b := Base{
		Range:   netip.MustParsePrefix("10.208.0.0/16"),
		Cluster: netip.MustParsePrefix("10.208.0.0/12"),
		WGIface: "wg0",
		Flows: []Flow{
			{Src: netip.MustParsePrefix("10.207.255.6/32"), VIP: netip.MustParseAddr("10.208.0.3"), Port: 80},
			{Src: netip.MustParsePrefix("10.208.1.0/24"), VIP: netip.MustParseAddr("10.213.0.9"), Port: 5432, Out: true},
		},
	}
	out := Render(nil, map[string]Entry{"10.208.0.3": {Replicas: []string{"10.208.0.4"}}}, b)
	for _, want := range []string{
		"-A STACKR-NAT -o wg0 -s 10.208.0.0/12 -j ACCEPT",
		"-A STACKR-RAW -i wg0 -s 10.208.0.0/12 -d 10.208.0.0/16 -j ACCEPT",
		"-A STACKR-RAW -i wg0 -s 10.207.255.0/24 -d 10.208.0.0/16 -j ACCEPT",
		"-A STACKR-FWD -i wg0 -j STACKR-XN",
		"-A STACKR-XN -i wg0 -s 10.207.255.6/32 -p tcp -m conntrack --ctstate NEW --ctorigdst 10.208.0.3 --ctorigdstport 80 -j ACCEPT",
		"-A STACKR-XN -s 10.208.1.0/24 -d 10.213.0.9/32 -p tcp --dport 5432 -j ACCEPT",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// A hop for a routed pair: mesh to mesh forwards, cluster addresses only,
	// ahead of the conntrack return so replies pass too.
	hop := "-A STACKR-FWD -i wg0 -o wg0 -s 10.207.255.0/24 -d 10.208.0.0/12 -j ACCEPT"
	if i, j := strings.Index(out, hop), strings.Index(out, "-A STACKR-FWD -m conntrack ! --ctstate NEW -j RETURN"); i < 0 || i > j {
		t.Errorf("mesh forward %q missing or after the conntrack return:\n%s", hop, out)
	}
	if strings.Count(out, "-i wg0 -o wg0") != 4 {
		t.Errorf("want 4 mesh forward rules:\n%s", out)
	}
	// no mesh, no cluster rules
	plain := Render(nil, nil, Base{Range: b.Range})
	if strings.Contains(plain, "wg0") {
		t.Errorf("single node renders mesh rules:\n%s", plain)
	}
}
