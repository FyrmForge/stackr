package v1

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
)

// The org, stack and env rows the API returns never carry protect_password.
func TestMaskSecrets(t *testing.T) {
	blob := `{"protect_password":"hunter2"}`
	for _, v := range []any{
		service.Org{Settings: blob}, []service.Org{{Settings: blob}},
		service.Stack{Settings: blob}, []service.Stack{{Settings: blob}},
		service.Environment{Settings: blob}, EnvOut{Environment: service.Environment{Settings: blob}},
		[]EnvOut{{Environment: service.Environment{Settings: blob}}},
	} {
		b, err := json.Marshal(maskSecrets(v))
		if err != nil || strings.Contains(string(b), "hunter2") {
			t.Errorf("%T leaks the password: %s %v", v, b, err)
		}
	}
}
