package login

import (
	"strconv"
	"testing"
)

// A full map refuses new keys and still counts the ones it holds; a key
// whose tries are all given back goes.
func TestTriesBounded(t *testing.T) {
	tr := &tries{m: map[string]window{}}
	for i := range maxKeys {
		if ok, _ := tr.take(strconv.Itoa(i), 10); !ok {
			t.Fatalf("key %d refused before the map is full", i)
		}
	}
	if ok, _ := tr.take("new", 10); ok || len(tr.m) != maxKeys {
		t.Errorf("full map took a new key: %v, %d keys", ok, len(tr.m))
	}
	if ok, _ := tr.take("0", 10); !ok {
		t.Error("full map refused a key it holds")
	}
	tr.give("1")
	if _, ok := tr.m["1"]; ok {
		t.Error("a key with no tries left stays")
	}
}
