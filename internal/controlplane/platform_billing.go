package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/controlplane/authz"
	"ebof-wg-mesh/internal/controlplane/billing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func withBilling(service *billing.Service, authorizer *authz.Authorizer) platformServiceOption {
	return func(s *platformService) {
		s.billingSvc = service
		if s.authz == nil {
			s.authz = authorizer
		}
	}
}

// billingWorkspace mints the workspace scope a billing operation needs.
// Reads take workspace read access; mutations take workspace admin.
func (s *platformService) billingWorkspace(ctx context.Context, workspaceID string, need authz.Access) (authz.Workspace, error) {
	user, err := authorizedUser(ctx)
	if err != nil {
		return authz.Workspace{}, err
	}
	if s.billingSvc == nil || s.authz == nil {
		return authz.Workspace{}, status.Error(codes.FailedPrecondition, "billing is not configured")
	}
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		return authz.Workspace{}, status.Error(codes.InvalidArgument, "workspace_id is required")
	}
	scope, err := s.authz.AuthorizeWorkspace(ctx, user, workspaceID, need)
	if err != nil {
		if errors.Is(err, authz.ErrDenied) {
			return authz.Workspace{}, status.Error(codes.PermissionDenied, "billing: workspace access denied")
		}
		return authz.Workspace{}, status.Errorf(codes.Internal, "billing authorize: %v", err)
	}
	return scope, nil
}

func billingStatusError(op string, err error) error {
	switch {
	case errors.Is(err, billing.ErrDisabled), errors.Is(err, billing.ErrStripeNotConfigured):
		return status.Errorf(codes.FailedPrecondition, "%s: %v", op, err)
	case errors.Is(err, billing.ErrInvalidInput):
		return status.Errorf(codes.InvalidArgument, "%s: %v", op, err)
	case errors.Is(err, billing.ErrNoAccount), errors.Is(err, sql.ErrNoRows):
		return status.Errorf(codes.NotFound, "%s: %v", op, err)
	case errors.Is(err, billing.ErrGrowthBlocked):
		return status.Errorf(codes.FailedPrecondition, "%s: %v", op, err)
	default:
		return status.Errorf(codes.Internal, "%s: %v", op, err)
	}
}

func (s *platformService) billingSummary(ctx context.Context, scope authz.Workspace) (*platformv1.BillingAccountSummary, error) {
	summary, err := s.billingSvc.AccountSummary(ctx, scope.ID())
	if err != nil {
		return nil, billingStatusError("billing account", err)
	}
	return toProtoBillingSummary(summary, s.billingSvc.Mode()), nil
}

func (s *platformService) GetBillingAccount(ctx context.Context, req *platformv1.GetBillingAccountRequest) (*platformv1.BillingAccountSummary, error) {
	scope, err := s.billingWorkspace(ctx, req.GetWorkspaceId(), authz.Read)
	if err != nil {
		return nil, err
	}
	return s.billingSummary(ctx, scope)
}

func (s *platformService) ListBillingStatements(ctx context.Context, req *platformv1.ListBillingStatementsRequest) (*platformv1.ListBillingStatementsResponse, error) {
	scope, err := s.billingWorkspace(ctx, req.GetWorkspaceId(), authz.Read)
	if err != nil {
		return nil, err
	}
	statements, err := s.billingSvc.ListStatements(ctx, scope.ID(), int(req.GetLimit()))
	if err != nil {
		return nil, billingStatusError("list billing statements", err)
	}
	resp := &platformv1.ListBillingStatementsResponse{}
	for _, st := range statements {
		resp.Statements = append(resp.Statements, toProtoBillingStatement(st, false))
	}
	return resp, nil
}

func (s *platformService) GetBillingStatement(ctx context.Context, req *platformv1.GetBillingStatementRequest) (*platformv1.BillingStatement, error) {
	scope, err := s.billingWorkspace(ctx, req.GetWorkspaceId(), authz.Read)
	if err != nil {
		return nil, err
	}
	statementID := strings.TrimSpace(req.GetStatementId())
	if statementID == "" {
		return nil, status.Error(codes.InvalidArgument, "statement_id is required")
	}
	st, err := s.billingSvc.GetStatement(ctx, scope.ID(), statementID)
	if err != nil {
		return nil, billingStatusError("billing statement", err)
	}
	return toProtoBillingStatement(st, req.GetIncludeCsv()), nil
}

func (s *platformService) UpdateBillingBudget(ctx context.Context, req *platformv1.UpdateBillingBudgetRequest) (*platformv1.BillingAccountSummary, error) {
	scope, err := s.billingWorkspace(ctx, req.GetWorkspaceId(), authz.Manage)
	if err != nil {
		return nil, err
	}
	if err := s.billingSvc.SetBudget(ctx, scope.ID(), req.GetBudgetStopCents()); err != nil {
		return nil, billingStatusError("update billing budget", err)
	}
	return s.billingSummary(ctx, scope)
}

func (s *platformService) ChangeBillingPlan(ctx context.Context, req *platformv1.ChangeBillingPlanRequest) (*platformv1.BillingAccountSummary, error) {
	scope, err := s.billingWorkspace(ctx, req.GetWorkspaceId(), authz.Manage)
	if err != nil {
		return nil, err
	}
	if err := s.billingSvc.ChangePlan(ctx, scope.ID(), req.GetPlanId()); err != nil {
		return nil, billingStatusError("change billing plan", err)
	}
	return s.billingSummary(ctx, scope)
}

func (s *platformService) StartBillingPaymentSetup(ctx context.Context, req *platformv1.StartBillingPaymentSetupRequest) (*platformv1.StartBillingPaymentSetupResponse, error) {
	scope, err := s.billingWorkspace(ctx, req.GetWorkspaceId(), authz.Manage)
	if err != nil {
		return nil, err
	}
	url, err := s.billingSvc.StartPaymentSetup(ctx, scope.ID())
	if err != nil {
		return nil, billingStatusError("billing payment setup", err)
	}
	return &platformv1.StartBillingPaymentSetupResponse{CheckoutUrl: url}, nil
}

func (s *platformService) StartBillingActivation(ctx context.Context, req *platformv1.StartBillingActivationRequest) (*platformv1.BillingAccountSummary, error) {
	scope, err := s.billingWorkspace(ctx, req.GetWorkspaceId(), authz.Manage)
	if err != nil {
		return nil, err
	}
	if err := s.billingSvc.StartPaidActivation(ctx, scope.ID(), req.GetPlanId()); err != nil {
		return nil, billingStatusError("billing activation", err)
	}
	return s.billingSummary(ctx, scope)
}

func (s *platformService) CancelBillingAccount(ctx context.Context, req *platformv1.CancelBillingAccountRequest) (*platformv1.CancelBillingAccountResponse, error) {
	scope, err := s.billingWorkspace(ctx, req.GetWorkspaceId(), authz.Manage)
	if err != nil {
		return nil, err
	}
	effective, err := s.billingSvc.Cancel(ctx, scope.ID())
	if err != nil {
		return nil, billingStatusError("cancel billing account", err)
	}
	return &platformv1.CancelBillingAccountResponse{EffectiveAt: timestamppb.New(effective)}, nil
}

// requireBillingGrowth refuses new resource consumption under an enforced
// billing restriction. Enforcement also lives in delivery (allocation
// creation, build claims, replica reconciliation); this check gives user
// requests an immediate, explicit error. It is a no-op outside enabled mode.
func (s *platformService) requireBillingGrowth(ctx context.Context, workspaceID string) error {
	if s.billingSvc == nil || workspaceID == "" {
		return nil
	}
	if err := s.billingSvc.CheckGrowth(ctx, workspaceID); err != nil {
		return status.Errorf(codes.FailedPrecondition, "%v", err)
	}
	return nil
}

func toProtoBillingSummary(summary billing.Summary, mode billing.Mode) *platformv1.BillingAccountSummary {
	account, period := summary.Account, summary.Period
	out := &platformv1.BillingAccountSummary{
		AccountId: account.ID, WorkspaceId: account.WorkspaceID, Currency: account.Currency,
		PlanId: account.PlanID, PendingPlanId: account.PendingPlanID, State: account.State,
		Mode: string(mode), PaymentReady: account.PaymentReady,
		PeriodId: period.ID, MonthlyMinimumCents: period.MonthlyMinimumCents,
		IncludedUsageCents: period.IncludedUsageCents,
		GrossCents:         summary.GrossCents, AfterAllowanceCents: summary.AfterAllowanceCents,
		CreditBalanceCents:        summary.CreditBalanceCents,
		EstimatedPeriodTotalCents: summary.EstimatedTotalCents,
		ForecastGrossCents:        summary.ForecastGrossCents,
		UnpaidCents:               summary.UnpaidCents,
		MeasuredMs:                summary.Coverage.MeasuredMs, GapMs: summary.Coverage.GapMs,
		UncertainMs: summary.Coverage.UncertainMs,
	}
	if account.BudgetStopCents.Valid {
		out.BudgetStopCents = account.BudgetStopCents.Int64
	}
	if account.TrialEndsAt.Valid {
		out.TrialEndsAt = timestamppb.New(account.TrialEndsAt.Time)
	}
	if account.CancelEffectiveAt.Valid {
		out.CancelEffectiveAt = timestamppb.New(account.CancelEffectiveAt.Time)
	}
	if period.ID != "" {
		out.PeriodStart = timestamppb.New(period.StartsAt)
		out.PeriodEnd = timestamppb.New(period.EndsAt)
	}
	if summary.Watermark.Valid {
		out.Watermark = timestamppb.New(summary.Watermark.Time)
	}
	out.Restriction = &platformv1.BillingRestrictionState{
		Restricted: summary.Restriction.Restricted, GrowthBlocked: summary.Restriction.GrowthBlocked,
		DryRun: summary.Restriction.DryRun, Reason: summary.Restriction.Reason,
	}
	if summary.Restriction.Since.Valid {
		out.Restriction.Since = timestamppb.New(summary.Restriction.Since.Time)
	}
	for _, cost := range summary.ProjectCosts {
		out.ProjectCosts = append(out.ProjectCosts, &platformv1.BillingProjectCost{
			ProjectId: cost.ProjectID, ProjectName: cost.ProjectName, Meter: string(cost.Meter),
			Quantity: cost.Quantity, AmountCents: cost.AmountCents,
		})
	}
	out.RateVersion = period.RateVersion
	for _, meter := range billing.Meters {
		rate, ok := summary.Rates.Rates[meter]
		if !ok {
			continue
		}
		out.Rates = append(out.Rates, &platformv1.BillingRate{
			Meter: string(meter), Unit: meter.Unit(),
			Numerator: rate.Num.String(), Denominator: rate.Den.String(),
		})
	}
	return out
}

func toProtoBillingStatement(st billing.Statement, includeCSV bool) *platformv1.BillingStatement {
	out := &platformv1.BillingStatement{
		Id: st.ID, PeriodId: st.PeriodID, Kind: st.Kind, Currency: st.Currency,
		Shadow: st.Shadow, State: st.State, GrossCents: st.GrossCents,
		IncludedCents: st.IncludedCents, AfterAllowanceCents: st.AfterAllowanceCents,
		CreditCents: st.CreditCents, MinimumCents: st.MinimumCents,
		PrepaidAppliedCents: st.PrepaidAppliedCents, NextMinimumCents: st.NextMinimumCents,
		PeriodTotalCents: st.PeriodTotalCents, AmountDueCents: st.AmountDueCents,
		StripeInvoiceId: st.StripeInvoiceID, StripeHostedInvoiceUrl: st.StripeHostedInvoiceURL,
		CreatedAt: timestamppb.New(st.CreatedAt),
	}
	for _, line := range st.Lines {
		proto := &platformv1.BillingStatementLine{
			ProjectId: line.ProjectID, ProjectName: line.ProjectName, Meter: line.Meter,
			Description: line.Description, Quantity: line.Quantity, Unit: line.Unit,
			RateNumerator: line.RateNumerator, RateDenominator: line.RateDenominator,
			AmountCents: line.AmountCents,
		}
		if line.PeriodStart.Valid {
			proto.PeriodStart = timestamppb.New(line.PeriodStart.Time)
			proto.PeriodEnd = timestamppb.New(line.PeriodEnd.Time)
		}
		out.Lines = append(out.Lines, proto)
	}
	if includeCSV {
		out.Csv = billing.StatementCSV(st)
	}
	return out
}

// IngestStripeWebhook records a forwarded Stripe delivery after signature
// verification. The console server route that forwards raw bytes here is
// browser-facing plumbing; financial validation stays in Go.
func (s *opsService) IngestStripeWebhook(ctx context.Context, req *platformv1.IngestStripeWebhookRequest) (*emptypb.Empty, error) {
	if s.billing == nil {
		return nil, status.Error(codes.FailedPrecondition, "billing is not configured")
	}
	err := s.billing.IngestWebhook(ctx, req.GetPayload(), req.GetSignature())
	switch {
	case err == nil:
		return &emptypb.Empty{}, nil
	case errors.Is(err, billing.ErrWebhookInvalidSignature):
		return nil, status.Error(codes.Unauthenticated, err.Error())
	case errors.Is(err, billing.ErrWebhookTooLarge):
		return nil, status.Error(codes.ResourceExhausted, err.Error())
	case errors.Is(err, billing.ErrStripeNotConfigured):
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	default:
		return nil, status.Errorf(codes.Internal, "ingest stripe webhook: %v", err)
	}
}

// SetBillingCollectionPaused flips the collection kill switch (operators).
func (s *opsService) SetBillingCollectionPaused(ctx context.Context, req *platformv1.SetBillingCollectionPausedRequest) (*platformv1.BillingControlState, error) {
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	if s.billing == nil || s.billingAuthz == nil {
		return nil, status.Error(codes.FailedPrecondition, "billing is not configured")
	}
	if _, err := s.billingAuthz.AuthorizeOperator(ctx, user); err != nil {
		return nil, status.Error(codes.PermissionDenied, "billing control: operator access required")
	}
	if err := s.billing.SetCollectionPaused(ctx, req.GetPaused(), req.GetReason()); err != nil {
		return nil, status.Errorf(codes.Internal, "set billing collection paused: %v", err)
	}
	paused, reason, err := s.billing.CollectionPaused(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read billing control: %v", err)
	}
	return &platformv1.BillingControlState{
		CollectionPaused: paused, PauseReason: reason, Mode: string(s.billing.Mode()),
	}, nil
}
