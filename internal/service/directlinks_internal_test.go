package service

import "testing"

// A routed pair is no line on the map; its hop goes on the card of the end
// with fewer working tunnels. A pair not judged yet stays drawn.
func TestDirectLinksDropRouted(t *testing.T) {
	nodes := []ClusterNode{{ID: "ctl"}, {ID: "h4"}, {ID: "h3"}, {ID: "ct"}}
	links := []ClusterLink{
		{A: "ctl", B: "h4"}, {A: "ctl", B: "ct"}, {A: "ct", B: "h4"}, {A: "h3", B: "h4"},
		{A: "ctl", B: "h3"}, {A: "ct", B: "h3"},
		{A: "ctl", B: "new"},
	}
	routed := map[string]bool{"ctl h3": true, "ct h3": true}
	direct := func(a, b string) (bool, bool) {
		if a == "new" || b == "new" {
			return false, false
		}
		return !routed[a+" "+b], true
	}
	via := func(a, b string) string {
		if routed[a+" "+b] {
			return "h4"
		}
		return ""
	}
	got := directLinks(links, nodes, map[string]string{"h4": "h4"}, direct, via)
	if len(got) != 5 {
		t.Fatalf("drawn %v, want the 4 direct pairs and the unjudged one", got)
	}
	for _, l := range got {
		if routed[l.A+" "+l.B] {
			t.Errorf("routed pair %s-%s drawn", l.A, l.B)
		}
	}
	if v := nodes[2].ReachedVia; len(v) != 1 || v[0] != "h4" {
		t.Errorf("h3 ReachedVia = %v, want [h4]", v)
	}
	for _, i := range []int{0, 1, 3} {
		if len(nodes[i].ReachedVia) != 0 {
			t.Errorf("%s ReachedVia = %v, want none", nodes[i].ID, nodes[i].ReachedVia)
		}
	}
}
