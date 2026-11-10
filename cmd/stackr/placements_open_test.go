package main

import "testing"

func TestOpenFlagNone(t *testing.T) {
	if all, orgs, err := openFlag("none"); err != nil || all || len(orgs) != 0 {
		t.Errorf("none = %v %v %v", all, orgs, err)
	}
	if all, _, _ := openFlag("all"); !all {
		t.Error("all")
	}
	if _, _, err := openFlag(" , "); err == nil {
		t.Error("blank list accepted")
	}
}
