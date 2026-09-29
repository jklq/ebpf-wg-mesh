package controlplane

import (
	"context"
	"errors"
	"testing"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	routingcore "ebof-wg-mesh/internal/controlplane/routing"
	sourcecore "ebof-wg-mesh/internal/controlplane/source"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

type GitHubCatalog = sourcecore.GitHubCatalog

type GitHubClient = sourcecore.GitHubClient

type GitHubCoordinator = sourcecore.GitHubCoordinator

type GitHubReconciler = sourcecore.GitHubReconciler

type GitHubWebhookHandler = sourcecore.GitHubWebhookHandler

type GitHubWebhookProcessor = sourcecore.GitHubWebhookProcessor

type gitHubAPIError = sourcecore.GitHubAPIError

type gitHubUserRepositoryAuthorizationError = sourcecore.GitHubUserRepositoryAuthorizationError

type noopWebhookProcessor struct{}

func (noopWebhookProcessor) RequestProcess() {}

var (
	NewGitHubClient                     = sourcecore.NewGitHubClient
	NewGitHubCatalog                    = sourcecore.NewGitHubCatalog
	NewGitHubCoordinator                = sourcecore.NewGitHubCoordinator
	NewGitHubReconciler                 = sourcecore.NewGitHubReconciler
	NewGitHubWebhookHandler             = sourcecore.NewGitHubWebhookHandler
	NewGitHubWebhookProcessor           = sourcecore.NewGitHubWebhookProcessor
	errGitHubUserAccessTokenRequired    = sourcecore.ErrGitHubUserAccessTokenRequired
	errGitHubRepositoryIdentityMismatch = sourcecore.ErrGitHubRepositoryIdentityMismatch
	errGitHubWebhookInvalidSignature    = sourcecore.ErrGitHubWebhookInvalidSignature
	errGitHubWebhookMissingHeaders      = sourcecore.ErrGitHubWebhookMissingHeaders
	errGitHubWebhookPayloadTooLarge     = sourcecore.ErrGitHubWebhookPayloadTooLarge
	maxGitHubWebhookPayloadBytes        = sourcecore.MaxWebhookPayloadBytes
	NewDomains                          = routingcore.NewDomains
)

func TestGitHubWebhookHandlerRejectsOversizedPayloadBeforeProcessing(t *testing.T) {
	t.Parallel()

	handler := &GitHubWebhookHandler{}
	payload := make([]byte, maxGitHubWebhookPayloadBytes+1)
	err := handler.HandleDelivery(context.Background(), "", "", "", payload)
	if !errors.Is(err, errGitHubWebhookPayloadTooLarge) {
		t.Fatalf("expected payload-too-large error, got %v", err)
	}
}

func TestGitHubUserAccessTokenFieldsAreMarkedDebugRedact(t *testing.T) {
	t.Parallel()

	const sentinel = "github-token-must-never-appear"
	messages := []struct {
		name    string
		message proto.Message
	}{
		{
			name: "link request",
			message: &platformv1.LinkGitHubRepositoryRequest{
				GithubUserAccessToken: sentinel,
			},
		},
		{
			name: "inspect request",
			message: &platformv1.InspectSourceRequest{
				GithubUserAccessToken: sentinel,
			},
		},
	}
	for _, test := range messages {
		t.Run(test.name, func(t *testing.T) {
			field := test.message.ProtoReflect().Descriptor().Fields().ByName(protoreflect.Name("github_user_access_token"))
			if field == nil {
				t.Fatal("github_user_access_token descriptor is missing")
			}
			options, ok := field.Options().(*descriptorpb.FieldOptions)
			if !ok || !options.GetDebugRedact() {
				t.Fatal("github_user_access_token is not marked debug_redact")
			}
		})
	}
}

func TestGitHubUserAuthorizationStatus(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name  string
		Cause error
		want  codes.Code
	}{
		{name: "missing token", Cause: errGitHubUserAccessTokenRequired, want: codes.Unauthenticated},
		{name: "expired token", Cause: &gitHubAPIError{StatusCode: 401}, want: codes.Unauthenticated},
		{name: "forbidden repository", Cause: &gitHubAPIError{StatusCode: 403}, want: codes.PermissionDenied},
		{name: "hidden repository", Cause: &gitHubAPIError{StatusCode: 404}, want: codes.PermissionDenied},
		{name: "repository identity mismatch", Cause: errGitHubRepositoryIdentityMismatch, want: codes.PermissionDenied},
		{name: "github server failure", Cause: &gitHubAPIError{StatusCode: 503}, want: codes.Unavailable},
		{name: "github transport failure", Cause: errors.New("connection reset"), want: codes.Unavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := gitHubUserAuthorizationStatus(&gitHubUserRepositoryAuthorizationError{Cause: test.Cause})
			if got := status.Code(err); got != test.want {
				t.Fatalf("status code = %s, want %s (error: %v)", got, test.want, err)
			}
		})
	}
}
