package controlplane

import (
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestAuthenticatedBuilderIDRejectsCertificateMismatch(t *testing.T) {
	t.Parallel()

	_, err := authenticatedBuilderID(ServiceCaller{Class: serviceCallerBuilder, ID: "builder-1"}, "builder-2")
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied, got %v", err)
	}

	id, err := authenticatedBuilderID(ServiceCaller{Class: serviceCallerBuilder, ID: "builder-1"}, "")
	if err != nil || id != "builder-1" {
		t.Fatalf("expected certificate builder ID, got id=%q err=%v", id, err)
	}
}

func TestValidateRuntimeImageRef(t *testing.T) {
	t.Parallel()
	pushRef := "registry.example.test:5000/platform/project/service:git-deadbeef"
	valid := "registry.example.test:5000/platform/project/service@sha256:" + strings.Repeat("a", 64)

	for name, tc := range map[string]struct {
		imageRef string
		wantErr  bool
	}{
		"assigned repository":   {imageRef: valid},
		"foreign repository":    {imageRef: "docker.io/evil/image@sha256:" + strings.Repeat("a", 64), wantErr: true},
		"tag instead of digest": {imageRef: "registry.example.test:5000/platform/project/service:latest", wantErr: true},
		"short digest":          {imageRef: "registry.example.test:5000/platform/project/service@sha256:abc", wantErr: true},
		"non-hex digest":        {imageRef: "registry.example.test:5000/platform/project/service@sha256:" + strings.Repeat("z", 64), wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			err := validateRuntimeImageRef(pushRef, tc.imageRef)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateRuntimeImageRef() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}
