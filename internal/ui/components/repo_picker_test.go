package components

import (
	"context"
	"strings"
	"testing"
)

// A repo two connectors read is offered once per connector, each option
// carrying its connector so the picker can follow the connector select.
func TestRepoPickerTagsConnector(t *testing.T) {
	var b strings.Builder
	v := RepoPickerView{ID: "x", Repos: []RepoOption{{FullName: "a/b", ConnectorID: "c1"}, {FullName: "a/b", ConnectorID: "c2"}}}
	if err := RepoPicker(v).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), `data-connector="c1"`) || !strings.Contains(b.String(), `data-connector="c2"`) {
		t.Errorf("options not tagged:\n%s", b.String())
	}
}
