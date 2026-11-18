package tile

import "testing"

func TestReasonLine(t *testing.T) {
	for name, c := range map[string]struct{ in, want string }{
		"error then info": {"Detected CPU Cores\nERROR: must be owner of database\ninfo: done\ninfo: bye", "ERROR: must be owner of database"},
		"json":            {`{"level":"info","msg":"a"}` + "\n" + `{"level":"error","msg":"boom"}` + "\n" + `{"level":"info","msg":"z"}`, `{"level":"error","msg":"boom"}`},
		"level field":     {"a\nlevel=fatal msg=x\nb", "level=fatal msg=x"},
		"none":            {"one\ntwo\nthree", "three"},
	} {
		if got := reasonLine(c.in); got != c.want {
			t.Errorf("%s: got %q want %q", name, got, c.want)
		}
	}
}
