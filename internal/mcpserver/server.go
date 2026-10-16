// Package mcpserver implements the platform MCP server on top of the
// public API (internal/apiclient only: the MCP server never talks to the
// control plane directly). It provides project, environment, service,
// variable, domain, volume, deployment, observability, billing, usage,
// and documentation tools with Railway-shaped ergonomics:
//
//   - resource IDs come from list tools, like Railway's;
//   - destructive tools are annotated destructiveHint and require a
//     preview call followed by confirm: true;
//   - service changes are staged drafts: mutating tools say so, and most
//     accept release: true to deploy immediately;
//   - linked defaults (workspace/project/environment/service) come from
//     environment variables, mirroring `railway link`.
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"

	"ebof-wg-mesh/internal/apiclient"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Defaults pre-select resources for tools whose arguments omit them,
// like a linked Railway project/environment/service.
type Defaults struct {
	WorkspaceID   string
	ProjectID     string
	EnvironmentID string
	ServiceID     string
}

// state is shared by every tool handler of one server instance. In HTTP
// mode one server (and client) is built per presented API token.
type state struct {
	client   *apiclient.Client
	defaults Defaults
	docs     *docIndex
}

// Version of the MCP server implementation.
const Version = "1.0.0"

// New builds an MCP server whose tools call the public API through
// client. The same server serves stdio and streamable HTTP transports.
func New(client *apiclient.Client, defaults Defaults) (*mcp.Server, error) {
	docs, err := loadDocs()
	if err != nil {
		return nil, err
	}
	s := &state{client: client, defaults: defaults, docs: docs}
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "ebpf-wg-mesh",
		Title:   "Platform MCP Server",
		Version: Version,
	}, &mcp.ServerOptions{
		Instructions: serverInstructions,
	})
	s.addWorkspaceTools(server)
	s.addProjectTools(server)
	s.addServiceTools(server)
	s.addDeployTools(server)
	s.addDomainVolumeTools(server)
	s.addObservabilityTools(server)
	s.addBillingDocsTools(server)
	s.addResources(server)
	s.addPrompts(server)
	return server, nil
}

const serverInstructions = `You manage a small PaaS through these tools.

Model: Workspace > Project > Environment > Service/Volume/DomainBinding.
Start with whoami, then list_projects / list_environments / list_services to discover IDs.

Service changes are STAGED DRAFTS: create_service, update_service, set_variables,
create_volume and scale changes do not affect running workloads until the environment
is released with release_environment (most mutating tools accept release:true to do
both). A service reports pending_changes and unapplied_changes while staged work waits.

Destructive tools (deletes, release_environment, billing cancel, deployment remove)
return a preview unless called with confirm:true. Show the preview to the user and
only confirm after they agree.

To debug a failing service, call diagnose_service first: it aggregates status,
deployments, build state, and recent logs with likely issues.

Volumes are single-node and not backed up; they grow but never shrink. One volume
mount per service. Generated domains come from generate_domain; custom domains need
DNS pointing at the platform (see domain_status ownership/certificate fields).`

// toolFunc is a tool implementation returning a JSON-renderable result
// (proto.Message, struct, or map) or an error.
type toolFunc[In any] func(ctx context.Context, in In) (any, error)

// add registers a typed tool with uniform result and error rendering.
func add[In any](server *mcp.Server, tool *mcp.Tool, fn toolFunc[In]) {
	mcp.AddTool(server, tool, func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		result, err := fn(ctx, in)
		if err != nil {
			return errorResult(err), nil, nil
		}
		rendered, err := renderResult(result)
		if err != nil {
			return errorResult(err), nil, nil
		}
		return rendered, nil, nil
	})
}

var marshalOpts = protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: false}

func renderResult(result any) (*mcp.CallToolResult, error) {
	var data []byte
	var err error
	switch value := result.(type) {
	case nil:
		data = []byte("{}")
	case string:
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: value}}}, nil
	case proto.Message:
		data, err = marshalOpts.Marshal(value)
	case json.RawMessage:
		data = value
	default:
		data, err = json.Marshal(value)
	}
	if err != nil {
		return nil, fmt.Errorf("render tool result: %w", err)
	}
	var structured any
	if err := json.Unmarshal(data, &structured); err != nil {
		structured = nil
	}
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: string(data)}},
		StructuredContent: structured,
	}, nil
}

func errorResult(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
		IsError: true,
	}
}

// protoJSON renders a proto message as a plain JSON value for embedding
// inside composite (map) tool results.
func protoJSON(msg proto.Message) any {
	if msg == nil {
		return nil
	}
	data, err := marshalOpts.Marshal(msg)
	if err != nil {
		return map[string]any{"unrenderable": err.Error()}
	}
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return string(data)
	}
	return value
}

func boolPtr(v bool) *bool { return &v }

// Annotation presets.
func readTool(name, description string) *mcp.Tool {
	return &mcp.Tool{Name: name, Description: description, Annotations: &mcp.ToolAnnotations{
		ReadOnlyHint: true, OpenWorldHint: boolPtr(false),
	}}
}

func writeTool(name, description string) *mcp.Tool {
	return &mcp.Tool{Name: name, Description: description, Annotations: &mcp.ToolAnnotations{
		DestructiveHint: boolPtr(false), OpenWorldHint: boolPtr(false),
	}}
}

func idempotentWriteTool(name, description string) *mcp.Tool {
	tool := writeTool(name, description)
	tool.Annotations.IdempotentHint = true
	return tool
}

func destructiveTool(name, description string) *mcp.Tool {
	return &mcp.Tool{Name: name, Description: description, Annotations: &mcp.ToolAnnotations{
		DestructiveHint: boolPtr(true), OpenWorldHint: boolPtr(false),
	}}
}

// Default resolution helpers.
func (s *state) workspaceID(given string) string {
	if given != "" {
		return given
	}
	return s.defaults.WorkspaceID
}

func (s *state) projectID(given string) string {
	if given != "" {
		return given
	}
	return s.defaults.ProjectID
}

func (s *state) environmentID(given string) string {
	if given != "" {
		return given
	}
	return s.defaults.EnvironmentID
}

func (s *state) serviceID(given string) string {
	if given != "" {
		return given
	}
	return s.defaults.ServiceID
}

func required(name, value string) error {
	if value == "" {
		return fmt.Errorf("%s is required (pass it or set the linked default)", name)
	}
	return nil
}
