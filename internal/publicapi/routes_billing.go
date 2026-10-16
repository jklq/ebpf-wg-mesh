package publicapi

import (
	"net/http"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

func (s *Server) billingRoutes() []routeEntry {
	read := scopeRule{minScope: tierWorkspace, kind: "workspace", param: "workspaceId"}
	write := scopeRule{minScope: tierAccount, kind: "workspace", param: "workspaceId", write: true}
	return []routeEntry{
		{method: "GET", pattern: "/v1/workspaces/{workspaceId}/billing", summary: "Billing account summary for the workspace", rule: read, resp: "platform.v1.BillingAccountSummary", handler: func(w http.ResponseWriter, r *http.Request) {
			resp, err := s.platform.GetBillingAccount(r.Context(), &platformv1.GetBillingAccountRequest{WorkspaceId: r.PathValue("workspaceId")})
			s.respond(w, r, http.StatusOK, resp, err)
		}},
		{method: "GET", pattern: "/v1/workspaces/{workspaceId}/billing/statements", summary: "List billing statements (limit)", rule: read, resp: "platform.v1.ListBillingStatementsResponse", handler: func(w http.ResponseWriter, r *http.Request) {
			limit, err := queryInt32(r, "limit")
			if err != nil {
				s.writeError(w, r, err)
				return
			}
			resp, err := s.platform.ListBillingStatements(r.Context(), &platformv1.ListBillingStatementsRequest{WorkspaceId: r.PathValue("workspaceId"), Limit: limit})
			s.respond(w, r, http.StatusOK, resp, err)
		}},
		{method: "GET", pattern: "/v1/workspaces/{workspaceId}/billing/statements/{statementId}", summary: "Get a billing statement (include_csv)", rule: read, resp: "platform.v1.BillingStatement", handler: func(w http.ResponseWriter, r *http.Request) {
			includeCSV, err := queryBool(r, "include_csv")
			if err != nil {
				s.writeError(w, r, err)
				return
			}
			resp, err := s.platform.GetBillingStatement(r.Context(), &platformv1.GetBillingStatementRequest{
				WorkspaceId: r.PathValue("workspaceId"), StatementId: r.PathValue("statementId"), IncludeCsv: includeCSV,
			})
			s.respond(w, r, http.StatusOK, resp, err)
		}},
		{method: "PATCH", pattern: "/v1/workspaces/{workspaceId}/billing/budget", summary: "Set the gross-usage budget stop in cents (0 clears)", rule: write, resp: "platform.v1.BillingAccountSummary", handler: func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				BudgetStopCents int64 `json:"budget_stop_cents"`
			}
			if err := s.decodeRawFields(w, r, &body); err != nil {
				s.writeError(w, r, err)
				return
			}
			resp, err := s.platform.UpdateBillingBudget(r.Context(), &platformv1.UpdateBillingBudgetRequest{WorkspaceId: r.PathValue("workspaceId"), BudgetStopCents: body.BudgetStopCents})
			s.respond(w, r, http.StatusOK, resp, err)
		}},
		{method: "POST", pattern: "/v1/workspaces/{workspaceId}/billing/plan", summary: "Change the billing plan", rule: write, resp: "platform.v1.BillingAccountSummary", handler: func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				PlanID string `json:"plan_id"`
			}
			if err := s.decodeRawFields(w, r, &body); err != nil {
				s.writeError(w, r, err)
				return
			}
			resp, err := s.platform.ChangeBillingPlan(r.Context(), &platformv1.ChangeBillingPlanRequest{WorkspaceId: r.PathValue("workspaceId"), PlanId: body.PlanID})
			s.respond(w, r, http.StatusOK, resp, err)
		}},
		{method: "POST", pattern: "/v1/workspaces/{workspaceId}/billing/payment-setup", summary: "Start payment method setup; returns a checkout URL", rule: write, resp: "platform.v1.StartBillingPaymentSetupResponse", handler: func(w http.ResponseWriter, r *http.Request) {
			resp, err := s.platform.StartBillingPaymentSetup(r.Context(), &platformv1.StartBillingPaymentSetupRequest{WorkspaceId: r.PathValue("workspaceId")})
			s.respond(w, r, http.StatusOK, resp, err)
		}},
		{method: "POST", pattern: "/v1/workspaces/{workspaceId}/billing/activation", summary: "Activate billing on a plan", rule: write, resp: "platform.v1.BillingAccountSummary", handler: func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				PlanID string `json:"plan_id"`
			}
			if err := s.decodeRawFields(w, r, &body); err != nil {
				s.writeError(w, r, err)
				return
			}
			resp, err := s.platform.StartBillingActivation(r.Context(), &platformv1.StartBillingActivationRequest{WorkspaceId: r.PathValue("workspaceId"), PlanId: body.PlanID})
			s.respond(w, r, http.StatusOK, resp, err)
		}},
		{method: "POST", pattern: "/v1/workspaces/{workspaceId}/billing/cancel", summary: "Cancel the billing account (effective at period end)", rule: write, resp: "platform.v1.CancelBillingAccountResponse", handler: func(w http.ResponseWriter, r *http.Request) {
			resp, err := s.platform.CancelBillingAccount(r.Context(), &platformv1.CancelBillingAccountRequest{WorkspaceId: r.PathValue("workspaceId")})
			s.respond(w, r, http.StatusOK, resp, err)
		}},
	}
}
