package cli

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/config/stackconf"
	v1 "github.com/FyrmForge/stackr/internal/stackrd/handlers/api/v1"
)

// TestWireContract pins the CLI's response types to the daemon's. The daemon's
// wire structs are unexported, but its OpenAPI spec is reflected straight off
// them, so every json tag the CLI decodes must appear in the matching schema,
// or a rename on either side has silently broken decoding to zero values.
//
// This imports internal/stackrd/handlers/api/v1, the one cross-binary import the per-binary
// layout otherwise forbids. Test-only, deliberate, and this file is the record
// of that.
func TestWireContract(t *testing.T) {
	a := v1.New(nil, nil, nil, nil, nil, nil, nil, nil, nil, stackconf.Applier{})
	a.Register(echo.New().Group("/api/v1")) // throwaway group; only the spec matters
	raw, err := a.SpecJSON()
	require.NoError(t, err)

	var spec struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	require.NoError(t, json.Unmarshal(raw, &spec))

	cases := []struct {
		schema string
		typ    any
	}{
		{"V1StackOut", Stack{}},
		{"V1EnvOut", Env{}},
		{"V1AppOut", App{}},
		{"V1DbOut", DB{}},
		{"V1ProvisionOut", Provision{}},
		{"V1VarEntry", Var{}},
		{"V1DomainOut", Domain{}},
		{"V1DomainResourceOut", DomainResource{}},
		{"V1VolumeOut", Volume{}},
		{"V1DestinationOut", Destination{}},
		{"V1BackupOut", Backup{}},
		{"V1BackupRunOut", BackupRun{}},
		{"V1PlanChangeOut", PlanChange{}},
		// Plan is the CLI's deliberate merge of the list header and the detail
		// body, so it checks against the detail schema (which embeds the header).
		{"V1PlanDetailOut", Plan{}},
		{"V1ResolveOut", Target{}},
		{"V1SliceOut", Slice{}},
	}
	for _, c := range cases {
		t.Run(c.schema, func(t *testing.T) {
			s, ok := spec.Components.Schemas[c.schema]
			require.True(t, ok, "schema %s missing from the daemon spec", c.schema)
			rt := reflect.TypeOf(c.typ)
			for i := 0; i < rt.NumField(); i++ {
				tag, _, _ := strings.Cut(rt.Field(i).Tag.Get("json"), ",")
				require.Contains(t, s.Properties, tag,
					"cli.%s field %s: json tag %q not in daemon schema %s",
					rt.Name(), rt.Field(i).Name, tag, c.schema)
			}
		})
	}

	// Fields the client decodes through anonymous structs.
	require.Contains(t, spec.Components.Schemas["V1DeployAccepted"].Properties, "deployment")
	require.Contains(t, spec.Components.Schemas["V1LogsOut"].Properties, "lines")
}
