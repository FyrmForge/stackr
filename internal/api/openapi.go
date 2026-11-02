package api

import (
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/api/handler/v1"
	"github.com/FyrmForge/stackr/internal/middleware"
)

// OpenAPI is the /api/v1 spec, read off the route table: paths, verbs,
// the gate, bodies from each endpoint's In and Out. Deterministic, so the
// checked-in copy diffs clean.
// ponytail: schemas say shape only (no enums, no formats beyond time); the
// verb's typed errors are the source of what a value may be.
func OpenAPI() ([]byte, error) {
	s := schemas{defs: map[string]any{}, names: map[string]reflect.Type{}}
	paths := map[string]map[string]any{}
	errBody := s.of(reflect.TypeFor[middleware.APIError]())
	for _, r := range Routes(&v1.H{}) {
		path, params := "/api/v1", []any{}
		for _, seg := range strings.Split(strings.TrimPrefix(r.Path, "/"), "/") {
			if name, ok := strings.CutPrefix(seg, ":"); ok {
				seg = "{" + name + "}"
				params = append(params, map[string]any{
					"name":     name,
					"in":       "path",
					"required": true,
					"schema":   map[string]any{"type": "string"},
				})
			}
			path += "/" + seg
		}
		for _, q := range r.E.Query {
			params = append(params, map[string]any{"name": q, "in": "query", "schema": querySchema(q)})
		}
		if r.E.Idem {
			params = append(params, map[string]any{
				"name":        middleware.IdempotencyHeader,
				"in":          "header",
				"description": "A retry with the same key, principal and body gets the first answer (kept 24 hours) instead of queuing a second job.",
				"schema":      map[string]any{"type": "string", "maxLength": 200},
			})
		}
		ok := map[string]any{"description": http.StatusText(r.E.Status)}
		code := itoa(r.E.Status)
		if slices.Contains(r.E.Query, "cursor") {
			ok["headers"] = map[string]any{"X-Next-Cursor": map[string]any{
				"description": "Present when more rows follow: pass it as ?cursor= with the same ?limit= for the next page.",
				"schema":      map[string]any{"type": "string"},
			}}
		}
		switch {
		case r.E.Stream == "websocket":
			code, ok = "101", map[string]any{"description": "Switching Protocols: the connection is now a websocket"}
		case r.E.Stream != "":
			ok["content"] = map[string]any{r.E.Stream: map[string]any{}}
		case r.E.Out != nil:
			ok["content"] = jsonBody(s.of(r.E.Out))
		}
		op := map[string]any{
			"operationId": r.Op,
			"x-verb":      string(r.Verb),
			"parameters":  params,
			"responses": map[string]any{
				code:      ok,
				"default": map[string]any{"description": "error", "content": jsonBody(errBody)},
			},
		}
		if r.Verb == Public {
			op["security"] = []any{}
		}
		if r.E.Note != "" {
			op["description"] = r.E.Note
		}
		if r.E.In != nil {
			body := s.of(r.E.In)
			if r.E.In == rawType { // a settings rung: an object of knob values
				body = map[string]any{"type": "object"}
			}
			optional := reflect.New(r.E.In).Interface()
			_, bodyOptional := optional.(interface{ BodyOptional() })
			op["requestBody"] = map[string]any{"required": !bodyOptional, "content": jsonBody(body)}
		}
		if r.E.InRaw != "" { // a stream's stdin is optional; an upload's body is not
			op["requestBody"] = map[string]any{"required": r.E.Stream == "", "content": map[string]any{r.E.InRaw: map[string]any{}}}
		}
		if paths[path] == nil {
			paths[path] = map[string]any{}
		}
		paths[path][strings.ToLower(r.Method)] = op
	}
	for path, ops := range outsideV1() {
		paths[path] = ops
	}
	return json.MarshalIndent(map[string]any{
		"openapi":  "3.1.0",
		"info":     map[string]any{"title": "stackr", "version": "v1"},
		"paths":    paths,
		"security": []any{map[string]any{"bearer": []any{}}},
		"components": map[string]any{
			"schemas":         s.defs,
			"securitySchemes": map[string]any{"bearer": map[string]any{"type": "http", "scheme": "bearer"}},
		},
	}, "", "  ")
}

// Query params by name: the handlers read them as numbers, flags or repeats.
var (
	intQuery  = map[string]bool{"limit": true, "offset": true, "tail": true, "port": true}
	boolQuery = map[string]bool{"all": true, "force": true}
	listQuery = map[string]bool{"state": true, "cmd": true} // repeated: ?state=a&state=b
)

func querySchema(name string) map[string]any {
	switch {
	case intQuery[name]:
		return map[string]any{"type": "integer"}
	case boolQuery[name]:
		return map[string]any{"type": "boolean"}
	case listQuery[name]:
		return map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
	}
	return map[string]any{"type": "string"}
}

// outsideV1 are the few routes that live beside /api/v1: they answer to
// something other than a bearer key, or are not JSON.
func outsideV1() map[string]map[string]any {
	op := func(id, summary string, public bool, status, media string) map[string]any {
		o := map[string]any{
			"operationId": id,
			"summary":     summary,
			"responses":   map[string]any{status: map[string]any{"description": http.StatusText(200), "content": map[string]any{media: map[string]any{}}}},
		}
		if public {
			o["security"] = []any{}
		}
		return o
	}
	return map[string]map[string]any{
		"/api/health": {"get": op("health", "Liveness: 200 while the panel serves.", true, "200", "application/json")},
		"/metrics":    {"get": op("metrics", "stackr's own Prometheus metrics (admin key or session).", false, "200", "text/plain")},
		"/hooks/connectors/{connector}": {"post": op("hook.connector",
			"A git host's push or webhook delivery for one connector, verified by its signature, not a key.", true, "200", "application/json")},
		"/join/{token}":                {"get": op("join.script", "The one-line join script (curl | sh) for a join token.", true, "200", "text/x-shellscript")},
		"/join/{token}/stackr-install": {"get": op("join.installer", "The stackr-install binary a join script downloads.", true, "200", "application/octet-stream")},
	}
}

func jsonBody(schema any) map[string]any {
	return map[string]any{"application/json": map[string]any{"schema": schema}}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

type schemas struct {
	defs  map[string]any
	names map[string]reflect.Type
}

var (
	timeType = reflect.TypeFor[time.Time]()
	rawType  = reflect.TypeFor[json.RawMessage]()
)

// of is t's schema: named structs go to components and come back as a $ref.
func (s *schemas) of(t reflect.Type) any {
	switch t {
	case timeType:
		return map[string]any{"type": "string", "format": "date-time"}
	case rawType:
		return map[string]any{}
	}
	switch t.Kind() {
	case reflect.Pointer:
		return map[string]any{"oneOf": []any{s.of(t.Elem()), map[string]any{"type": "null"}}}
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			return map[string]any{"type": "string"}
		}
		return map[string]any{"type": "array", "items": s.of(t.Elem())}
	case reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": s.of(t.Elem())}
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.Struct:
		name := s.name(t)
		if _, done := s.defs[name]; !done {
			s.defs[name] = nil // a cycle refers back instead of recursing
			s.defs[name] = s.object(t)
		}
		return map[string]any{"$ref": "#/components/schemas/" + name}
	}
	return map[string]any{}
}

// name is the type's name, qualified by its package when two differ.
func (s *schemas) name(t reflect.Type) string {
	name := t.Name()
	if name == "" {
		name = "Anon"
	}
	if prev, ok := s.names[name]; ok && prev != t {
		pkg := t.PkgPath()[strings.LastIndex(t.PkgPath(), "/")+1:]
		name = strings.ToUpper(pkg[:1]) + pkg[1:] + name // promote.Plan and serverconfig.Plan: PromotePlan, ServerconfigPlan
	}
	s.names[name] = t
	return name
}

func (s *schemas) object(t reflect.Type) map[string]any {
	props, required := map[string]any{}, []string{}
	var walk func(t reflect.Type, depth int)
	walk = func(t reflect.Type, depth int) {
		var embedded []reflect.Type
		for i := range t.NumField() {
			f := t.Field(i)
			tag := f.Tag.Get("json")
			name, opts, _ := strings.Cut(tag, ",")
			if name == "-" || !f.IsExported() {
				continue
			}
			if f.Anonymous && name == "" && f.Type.Kind() == reflect.Struct {
				embedded = append(embedded, f.Type) // after this struct's own fields, which win a clash
				continue
			}
			if name == "" {
				name = f.Name
			}
			prop := s.of(f.Type)
			if f.Tag.Get("api") == "required" { // a pointer the API refuses when missing or null
				prop = s.of(f.Type.Elem())
			}
			if enum := f.Tag.Get("enum"); enum != "" {
				if m, ok := prop.(map[string]any); ok && m["type"] == "string" {
					m["enum"] = strings.Split(enum, ",")
				}
			}
			if _, shadowed := props[name]; shadowed && depth > 0 {
				continue // a field of the outer struct hides the embedded one of the same name
			}
			props[name] = prop
			if f.Tag.Get("api") == "required" || (!strings.Contains(opts, "omitempty") && f.Type.Kind() != reflect.Pointer) {
				required = append(required, name)
			}
		}
		for _, e := range embedded {
			walk(e, depth+1)
		}
	}
	walk(t, 0)
	o := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		o["required"] = required
	}
	return o
}
