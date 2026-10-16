package publicapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// The OpenAPI document is generated from the route table and the protobuf
// descriptors, so it cannot drift from the implementation: schemas are
// the JSON mapping of the RPC messages (UseProtoNames, int64 as string).

var (
	openAPICache []byte
	openAPIOnce  sync.Once
	openAPIErr   error
)

func (s *Server) writeOpenAPI(w http.ResponseWriter, r *http.Request) {
	openAPIOnce.Do(func() {
		openAPICache, openAPIErr = buildOpenAPI(s.allRoutes())
	})
	if openAPIErr != nil {
		s.writeError(w, r, openAPIErr)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(openAPICache)
}

func buildOpenAPI(routes []routeEntry) ([]byte, error) {
	gen := &schemaGen{schemas: map[string]any{}, building: map[string]bool{}}
	paths := map[string]any{}
	for _, entry := range routes {
		op := map[string]any{
			"summary":   strings.TrimSpace(strings.Split(entry.summary, "(")[0]),
			"responses": map[string]any{},
		}
		if entry.rule.write {
			op["x-access"] = "write"
		} else {
			op["x-access"] = "read"
		}
		if entry.rule.minScope != tierAny {
			op["x-min-token-scope"] = entry.rule.minScope
		}
		var params []any
		for _, segment := range strings.Split(entry.pattern, "/") {
			if strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "}") {
				params = append(params, map[string]any{
					"name": strings.Trim(segment, "{}"), "in": "path", "required": true,
					"schema": map[string]any{"type": "string"},
				})
			}
		}
		for _, name := range summaryQueryParams(entry.summary) {
			params = append(params, map[string]any{
				"name": snakeParam(name), "in": "query", "required": false,
				"schema": queryParamSchema(snakeParam(name)),
			})
		}
		if len(params) > 0 {
			op["parameters"] = params
		}
		if entry.req != "" {
			schema, err := gen.ref(entry.req)
			if err != nil {
				return nil, err
			}
			op["requestBody"] = map[string]any{
				"required": true,
				"content":  map[string]any{"application/json": map[string]any{"schema": schema}},
			}
		}
		responses := op["responses"].(map[string]any)
		success := "200"
		if entry.method == "POST" && entry.req != "" {
			success = "201"
		}
		if entry.resp == "" {
			responses["204"] = map[string]any{"description": "No content (or non-proto JSON for meta endpoints)"}
		} else {
			schema, err := gen.ref(entry.resp)
			if err != nil {
				return nil, err
			}
			responses[success] = map[string]any{
				"description": "OK",
				"content":     map[string]any{"application/json": map[string]any{"schema": schema}},
			}
		}
		responses["default"] = map[string]any{
			"description": "Error",
			"content": map[string]any{"application/json": map[string]any{
				"schema": map[string]any{"$ref": "#/components/schemas/Error"},
			}},
		}
		pathItem, _ := paths[entry.pattern].(map[string]any)
		if pathItem == nil {
			pathItem = map[string]any{}
			paths[entry.pattern] = pathItem
		}
		pathItem[strings.ToLower(entry.method)] = op
	}
	gen.schemas["Error"] = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"error": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"code":       map[string]any{"type": "string"},
					"message":    map[string]any{"type": "string"},
					"request_id": map[string]any{"type": "string"},
				},
				"required": []string{"code", "message", "request_id"},
			},
		},
		"required": []string{"error"},
	}
	doc := map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":   "Platform Public API",
			"version": "1",
			"description": "Versioned REST/JSON API authenticated with Bearer API tokens " +
				"(account, workspace, or project scopes). Bodies are the protobuf JSON mapping of " +
				"the platform.v1 messages. Service and volume changes are staged drafts until the " +
				"environment is released. See docs/public-api.md.",
		},
		"servers":  []any{map[string]any{"url": "/"}},
		"security": []any{map[string]any{"bearerAuth": []any{}}},
		"components": map[string]any{
			"securitySchemes": map[string]any{
				"bearerAuth": map[string]any{"type": "http", "scheme": "bearer", "description": "API token secret (wgm_...)"},
			},
			"schemas": gen.schemas,
		},
		"paths": paths,
	}
	return json.MarshalIndent(doc, "", "  ")
}

func summaryQueryParams(summary string) []string {
	open := strings.LastIndex(summary, "(")
	if open < 0 || !strings.HasSuffix(summary, ")") {
		return nil
	}
	inside := summary[open+1 : len(summary)-1]
	var out []string
	for _, part := range strings.Split(inside, ",") {
		part = strings.TrimSpace(part)
		if part != "" && !strings.Contains(part, " ") {
			out = append(out, part)
		}
	}
	return out
}

func snakeParam(name string) string { return name }

func queryParamSchema(name string) map[string]any {
	switch name {
	case "include_deleted", "include_summaries", "resource_buckets", "include_csv":
		return map[string]any{"type": "boolean"}
	case "limit":
		return map[string]any{"type": "integer"}
	case "start_time", "end_time":
		return map[string]any{"type": "string", "format": "date-time"}
	default:
		return map[string]any{"type": "string"}
	}
}

type schemaGen struct {
	schemas  map[string]any
	building map[string]bool
}

// ref returns a $ref for a full proto message name, generating its schema
// (and transitive schemas) on first use.
func (g *schemaGen) ref(fullName string) (map[string]any, error) {
	short := shortName(fullName)
	if _, ok := g.schemas[short]; !ok && !g.building[short] {
		g.building[short] = true
		schema, err := g.messageSchema(fullName)
		if err != nil {
			return nil, err
		}
		g.schemas[short] = schema
		delete(g.building, short)
	}
	return map[string]any{"$ref": "#/components/schemas/" + short}, nil
}

func shortName(fullName string) string {
	if idx := strings.LastIndex(fullName, "."); idx >= 0 {
		return fullName[idx+1:]
	}
	return fullName
}

func (g *schemaGen) messageSchema(fullName string) (map[string]any, error) {
	desc, err := protoregistry.GlobalTypes.FindMessageByName(protoreflect.FullName(fullName))
	if err != nil {
		// Fall back to the files registry for generated _entry types etc.
		return nil, fmt.Errorf("openapi: message %s: %w", fullName, err)
	}
	msg := desc.Descriptor()
	props := map[string]any{}
	fields := msg.Fields()
	names := make([]string, 0, fields.Len())
	for i := range fields.Len() {
		field := fields.Get(i)
		schema, err := g.fieldSchema(field)
		if err != nil {
			return nil, err
		}
		// The wire format uses proto field names (UseProtoNames).
		props[string(field.Name())] = schema
		names = append(names, string(field.Name()))
	}
	sort.Strings(names)
	return map[string]any{"type": "object", "properties": props, "additionalProperties": false}, nil
}

func (g *schemaGen) fieldSchema(field protoreflect.FieldDescriptor) (map[string]any, error) {
	if field.IsMap() {
		value, err := g.singularSchema(field.MapValue())
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "object", "additionalProperties": value}, nil
	}
	single, err := g.singularSchema(field)
	if err != nil {
		return nil, err
	}
	if field.IsList() {
		return map[string]any{"type": "array", "items": single}, nil
	}
	return single, nil
}

func (g *schemaGen) singularSchema(field protoreflect.FieldDescriptor) (map[string]any, error) {
	switch field.Kind() {
	case protoreflect.BoolKind:
		return map[string]any{"type": "boolean"}, nil
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return map[string]any{"type": "integer", "format": "int32"}, nil
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind,
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		// protojson renders 64-bit integers as strings.
		return map[string]any{"type": "string", "format": "int64"}, nil
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		return map[string]any{"type": "number"}, nil
	case protoreflect.StringKind:
		return map[string]any{"type": "string"}, nil
	case protoreflect.BytesKind:
		return map[string]any{"type": "string", "format": "byte"}, nil
	case protoreflect.EnumKind:
		values := field.Enum().Values()
		names := make([]string, 0, values.Len())
		for i := range values.Len() {
			names = append(names, string(values.Get(i).Name()))
		}
		return map[string]any{"type": "string", "enum": names}, nil
	case protoreflect.MessageKind, protoreflect.GroupKind:
		full := string(field.Message().FullName())
		switch full {
		case "google.protobuf.Timestamp":
			return map[string]any{"type": "string", "format": "date-time"}, nil
		case "google.protobuf.Duration":
			return map[string]any{"type": "string"}, nil
		case "google.protobuf.Empty":
			return map[string]any{"type": "object"}, nil
		}
		return g.ref(full)
	default:
		return map[string]any{}, nil
	}
}
