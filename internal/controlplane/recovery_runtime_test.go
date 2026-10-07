package controlplane

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestRecoveryPauseRejectsAllMutationTransports(t *testing.T) {
	for _, method := range []string{"/platform.v1.PlatformService/CreateService", "/platform.v1.OpsService/IngestGitHubWebhook", "/platform.v1.BuilderService/ClaimBuild", "/agent.v1.AgentControl/Enroll", "/platform.v1.OpsService/SetAgentLifecycle", "/platform.v1.PlatformService/FutureMutation"} {
		t.Run(method, func(t *testing.T) {
			called := false
			_, err := recoveryUnary(true)(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: method}, func(context.Context, any) (any, error) { called = true; return nil, nil })
			if called || status.Code(err) != codes.Unavailable {
				t.Fatal("paused RPC executed", called, err)
			}
			response := httptest.NewRecorder()
			recoveryHTTP(true, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })).ServeHTTP(response, httptest.NewRequest("POST", method, nil))
			if called || response.Code != http.StatusServiceUnavailable {
				t.Fatal("paused Connect mutation executed")
			}
		})
	}
	for _, method := range []string{"/platform.v1.PlatformService/ListServices", "/platform.v1.OpsService/ListFleet"} {
		called := false
		_, err := recoveryUnary(true)(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: method}, func(context.Context, any) (any, error) { called = true; return nil, nil })
		if !called || err != nil {
			t.Fatal("operator inspection unavailable", method, err)
		}
	}
}
