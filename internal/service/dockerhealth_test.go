package service

import (
	"context"
	"errors"
	"testing"
)

func TestDockerDownIsAProblemUntilItAnswers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newWorld(t)
	w.fake.Err = map[string]error{"HostInfo": errors.New("cannot connect")}
	w.orch.dockerAlert(ctx)
	if !w.orch.DockerDown(ctx) {
		t.Fatal("a silent daemon opened no problem")
	}
	if ps, _ := w.orch.Problems(ctx); len(ps) == 0 {
		t.Error("the banner does not list it")
	}
	w.fake.Err = nil
	w.orch.dockerAlert(ctx)
	if w.orch.DockerDown(ctx) {
		t.Error("an answering daemon kept the problem")
	}
}
