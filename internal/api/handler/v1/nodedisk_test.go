package v1

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
)

// The forget plan tells the CLI and clients that logs and metrics are on the disk.
func TestForgetPlanOutCarriesObserveOn(t *testing.T) {
	b, err := json.Marshal(forgetPlanOut(service.ForgetPlan{DiskID: "d1", ObserveOn: true}))
	if err != nil || !strings.Contains(string(b), `"observe_on":true`) {
		t.Errorf("plan json: %s %v", b, err)
	}
}
