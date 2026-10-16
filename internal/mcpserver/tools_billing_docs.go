package mcpserver

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func (s *state) addBillingDocsTools(server *mcp.Server) {
	add(server, readTool("get_billing_account", "Billing account for a workspace: plan, mode (shadow never charges), period totals, forecast, restrictions, per-project costs."), func(ctx context.Context, in workspaceIDIn) (any, error) {
		id := s.workspaceID(in.WorkspaceID)
		if err := required("workspace_id", id); err != nil {
			return nil, err
		}
		return s.client.GetBillingAccount(ctx, id)
	})

	type statementsIn struct {
		WorkspaceID string `json:"workspace_id"`
		Limit       int32  `json:"limit,omitempty"`
	}
	add(server, readTool("list_billing_statements", "List frozen billing statements for a workspace."), func(ctx context.Context, in statementsIn) (any, error) {
		id := s.workspaceID(in.WorkspaceID)
		if err := required("workspace_id", id); err != nil {
			return nil, err
		}
		return s.client.ListBillingStatements(ctx, id, in.Limit)
	})

	type statementIn struct {
		WorkspaceID string `json:"workspace_id"`
		StatementID string `json:"statement_id"`
		IncludeCSV  bool   `json:"include_csv,omitempty"`
	}
	add(server, readTool("get_billing_statement", "Get one billing statement with lines (and optional CSV rendering)."), func(ctx context.Context, in statementIn) (any, error) {
		id := s.workspaceID(in.WorkspaceID)
		if err := required("workspace_id", id); err != nil {
			return nil, err
		}
		return s.client.GetBillingStatement(ctx, id, in.StatementID, in.IncludeCSV)
	})

	type budgetIn struct {
		WorkspaceID     string `json:"workspace_id"`
		BudgetStopCents int64  `json:"budget_stop_cents" jsonschema:"gross-usage stop threshold in cents; 0 clears. Minimums and retained storage still bill"`
	}
	add(server, idempotentWriteTool("update_billing_budget", "Set the workspace gross-usage budget stop."), func(ctx context.Context, in budgetIn) (any, error) {
		id := s.workspaceID(in.WorkspaceID)
		if err := required("workspace_id", id); err != nil {
			return nil, err
		}
		return s.client.UpdateBillingBudget(ctx, id, in.BudgetStopCents)
	})

	type planIn struct {
		WorkspaceID string `json:"workspace_id"`
		PlanID      string `json:"plan_id"`
	}
	add(server, writeTool("change_billing_plan", "Change the workspace billing plan."), func(ctx context.Context, in planIn) (any, error) {
		id := s.workspaceID(in.WorkspaceID)
		if err := required("workspace_id", id); err != nil {
			return nil, err
		}
		return s.client.ChangeBillingPlan(ctx, id, in.PlanID)
	})

	add(server, writeTool("start_billing_payment_setup", "Start payment-method setup; returns a hosted checkout URL to give the user."), func(ctx context.Context, in workspaceIDIn) (any, error) {
		id := s.workspaceID(in.WorkspaceID)
		if err := required("workspace_id", id); err != nil {
			return nil, err
		}
		return s.client.StartBillingPaymentSetup(ctx, id)
	})

	type cancelBillingIn struct {
		WorkspaceID string `json:"workspace_id"`
		Confirm     bool   `json:"confirm,omitempty"`
	}
	add(server, destructiveTool("cancel_billing_account", "Cancel the workspace billing account (effective at period end). Destructive: preview first, then confirm:true."), func(ctx context.Context, in cancelBillingIn) (any, error) {
		id := s.workspaceID(in.WorkspaceID)
		if err := required("workspace_id", id); err != nil {
			return nil, err
		}
		if !in.Confirm {
			account, err := s.client.GetBillingAccount(ctx, id)
			if err != nil {
				return nil, err
			}
			return preview(fmt.Sprintf("cancel billing for workspace %s (state %s, plan %s)", id, account.GetState(), account.GetPlanId()), map[string]any{"account": protoJSON(account)}), nil
		}
		return s.client.CancelBillingAccount(ctx, id)
	})

	type docsSearchIn struct {
		Query string `json:"query" jsonschema:"keywords, e.g. 'volume backup' or 'custom domain certificate'"`
		Limit int    `json:"limit,omitempty" jsonschema:"max results (default 5)"`
	}
	add(server, readTool("docs_search", "Search the embedded platform documentation (billing, observability, resource usage, storage operations, MCP and public API guides)."), func(ctx context.Context, in docsSearchIn) (any, error) {
		limit := in.Limit
		if limit <= 0 {
			limit = 5
		}
		hits := s.docs.search(in.Query, limit)
		return map[string]any{"query": in.Query, "results": hits, "documents": s.docs.names}, nil
	})

	type docsFetchIn struct {
		Name string `json:"name" jsonschema:"document name from docs_search results, e.g. billing.md or resource-usage"`
	}
	add(server, readTool("docs_fetch", "Fetch one embedded documentation file in full."), func(ctx context.Context, in docsFetchIn) (any, error) {
		text, ok := s.docs.fetch(in.Name)
		if !ok {
			return nil, fmt.Errorf("unknown document %q; available: %s", in.Name, strings.Join(s.docs.names, ", "))
		}
		return text, nil
	})
}

// addResources exposes each doc as a readable MCP resource.
func (s *state) addResources(server *mcp.Server) {
	for _, name := range s.docs.names {
		name := name
		server.AddResource(&mcp.Resource{
			URI:         "platform://docs/" + url.PathEscape(name),
			Name:        name,
			Title:       "Documentation: " + name,
			Description: "Embedded platform documentation",
			MIMEType:    "text/markdown",
		}, func(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			text, ok := s.docs.fetch(name)
			if !ok {
				return nil, fmt.Errorf("document %q not found", name)
			}
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{
				{URI: req.Params.URI, MIMEType: "text/markdown", Text: text},
			}}, nil
		})
	}
}

func (s *state) addPrompts(server *mcp.Server) {
	server.AddPrompt(&mcp.Prompt{
		Name:        "deploy_app",
		Title:       "Deploy an app",
		Description: "Guided flow: create project/service, configure, release, assign a domain",
		Arguments: []*mcp.PromptArgument{
			{Name: "app_description", Description: "what to deploy (image or repository, ports, env)", Required: true},
			{Name: "project_name", Description: "project name to create or reuse"},
		},
	}, func(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		args := req.Params.Arguments
		text := fmt.Sprintf(`Deploy this app to the platform: %s

Steps:
1. whoami to confirm the token, then list_projects (create project %q if missing).
2. Pick or create the target environment with list_environments.
3. create_service with the image or repository, ports, env, and resources; add a volume first if the app needs persistence.
4. Show the user the release_environment preview, then release with confirm:true.
5. Poll get_service_status and list_deployments until healthy; on failure use diagnose_service.
6. generate_domain (or create_domain for a custom hostname) and report the URL.
`, args["app_description"], args["project_name"])
		return &mcp.GetPromptResult{
			Description: "Deploy an app",
			Messages:    []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: text}}},
		}, nil
	})

	server.AddPrompt(&mcp.Prompt{
		Name:        "debug_deployment",
		Title:       "Debug a failing deployment",
		Description: "Evidence-first debugging flow built on diagnose_service",
		Arguments: []*mcp.PromptArgument{
			{Name: "service_id", Description: "service to debug", Required: true},
			{Name: "symptom", Description: "what the user observes"},
		},
	}, func(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		args := req.Params.Arguments
		text := fmt.Sprintf(`Debug service %s. Symptom: %s

1. Call diagnose_service for the service and read likely_issues first.
2. Distinguish build failure (latest_build state/failure_reason, build_logs) from runtime failure (deployment state/reason, allocation restart crash-loop evidence, runtime_logs).
3. Check pending_changes: the fix may be staged but unreleased.
4. Propose the smallest fix, apply it with the matching tool, preview the release, and confirm with the user before releasing.
5. Verify with get_service_status and fresh logs; report the deployment state you observed.
`, args["service_id"], args["symptom"])
		return &mcp.GetPromptResult{
			Description: "Debug a failing deployment",
			Messages:    []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: text}}},
		}, nil
	})
}
