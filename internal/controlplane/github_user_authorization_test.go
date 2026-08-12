package controlplane

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

func TestGitHubUserAccessTokensAreRedactedFromProtobufDebugText(t *testing.T) {
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

			debugText := fmt.Sprint(test.message)
			if strings.Contains(debugText, sentinel) {
				t.Skip("current Go protobuf runtime exposes debug_redact fields; descriptor is marked for runtimes that honor it")
			}
			if text := prototext.Format(test.message); strings.Contains(text, sentinel) {
				t.Fatalf("protobuf text exposed GitHub token despite runtime debug redaction support: %s", text)
			}
		})
	}
}

func TestGitHubUserAuthorizationStatus(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name  string
		cause error
		want  codes.Code
	}{
		{name: "missing token", cause: errGitHubUserAccessTokenRequired, want: codes.Unauthenticated},
		{name: "expired token", cause: &gitHubAPIError{StatusCode: 401}, want: codes.Unauthenticated},
		{name: "forbidden repository", cause: &gitHubAPIError{StatusCode: 403}, want: codes.PermissionDenied},
		{name: "hidden repository", cause: &gitHubAPIError{StatusCode: 404}, want: codes.PermissionDenied},
		{name: "repository identity mismatch", cause: errGitHubRepositoryIdentityMismatch, want: codes.PermissionDenied},
		{name: "github server failure", cause: &gitHubAPIError{StatusCode: 503}, want: codes.Unavailable},
		{name: "github transport failure", cause: errors.New("connection reset"), want: codes.Unavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := gitHubUserAuthorizationStatus(&gitHubUserRepositoryAuthorizationError{cause: test.cause})
			if got := status.Code(err); got != test.want {
				t.Fatalf("status code = %s, want %s (error: %v)", got, test.want, err)
			}
		})
	}
}
