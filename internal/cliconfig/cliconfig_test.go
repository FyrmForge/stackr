package cliconfig

import (
	"reflect"
	"testing"
)

func TestUseRoundTrip(t *testing.T) {
	c := Config{Server: "a", Key: "ka", Org: "oa"}
	w, ok := c.Use("work", true)
	if !ok || w.Name() != "work" || w.Server != "" || w.Profiles[Default].Server != "a" {
		t.Fatalf("switch: %+v", w)
	}
	w.Server = "b"
	back, ok := w.Use(Default, false)
	if !ok || back.Server != "a" || back.Profile != "" || back.Profiles["work"].Server != "b" {
		t.Fatalf("back: %+v", back)
	}
	if _, ok := back.Use("nope", false); ok {
		t.Fatal("missing profile switched")
	}
	if got := back.Names(); !reflect.DeepEqual(got, []string{Default, "work"}) {
		t.Fatalf("names %v", got)
	}
}
