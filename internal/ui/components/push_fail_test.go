package components

import (
	"context"
	"strings"
	"testing"
)

func TestPushFailedNoDoubleStop(t *testing.T) {
	var b strings.Builder
	if err := PushFailed(NewPushFail("remote: denied.", "/x")).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	if got := b.String(); strings.Contains(got, "..") || !strings.Contains(got, "denied.<") {
		t.Errorf("line = %s", got)
	}
}
