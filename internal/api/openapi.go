package api

import (
	"encoding/json"
	"net/http"
	"reflect"
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
			params = append(params, map[string]any{
				"name":   q,
				"in":     "query",
				"schema": map[string]any{"type": "string"},
			})
		}
		ok := map[string]any{"description": http.StatusText(r.E.Status)}
		switch {
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
				itoa(r.E.Status): ok,
				"default":        map[string]any{"description": "error", "content": jsonBody(errBody)},
			},
		}
		if r.Verb == Public {
			op["security"] = []any{}
		}
		if r.E.In != nil {
			op["requestBody"] = map[string]any{"required": true, "content": jsonBody(s.of(r.E.In))}
		}
		if paths[path] == nil {
			paths[path] = map[string]any{}
		}
		paths[path][strings.ToLower(r.Method)] = op
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
		name = t.PkgPath()[strings.LastIndex(t.PkgPath(), "/")+1:] + "." + name
	}
	s.names[name] = t
	return name
}

func (s *schemas) object(t reflect.Type) map[string]any {
	props, required := map[string]any{}, []string{}
	var walk func(t reflect.Type)
	walk = func(t reflect.Type) {
		for i := range t.NumField() {
			f := t.Field(i)
			tag := f.Tag.Get("json")
			name, opts, _ := strings.Cut(tag, ",")
			if name == "-" || !f.IsExported() {
				continue
			}
			if f.Anonymous && name == "" && f.Type.Kind() == reflect.Struct {
				walk(f.Type)
				continue
			}
			if name == "" {
				name = f.Name
			}
			props[name] = s.of(f.Type)
			if !strings.Contains(opts, "omitempty") && f.Type.Kind() != reflect.Pointer {
				required = append(required, name)
			}
		}
	}
	walk(t)
	o := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		o["required"] = required
	}
	return o
}
