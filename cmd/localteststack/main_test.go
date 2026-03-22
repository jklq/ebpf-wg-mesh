package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestDescribeOnePasswordLoadErrorForDesktopAccountMismatch(t *testing.T) {
	t.Parallel()

	message := describeOnePasswordLoadError(errors.New("initialize 1Password client: error initializing client: An error occurred when processing SDK request: Error { msg: Account not found, inner: None }"))
	if !strings.Contains(message, "desktop-app auth could not find OP_ACCOUNT") {
		t.Fatalf("unexpected message %q", message)
	}
	if !strings.Contains(message, "OP_SERVICE_ACCOUNT_TOKEN") {
		t.Fatalf("expected service-account guidance in %q", message)
	}
}

func TestDescribeNgrokStartupErrorForInvalidAuthtoken(t *testing.T) {
	t.Parallel()

	message := describeNgrokStartupError(errors.New("start ngrok tunnel: failed to connect: ERR_NGROK_107"), "mesh.example.test")
	if !strings.Contains(message, "invalid NGROK_AUTHTOKEN") {
		t.Fatalf("unexpected message %q", message)
	}
	if !strings.Contains(message, "mesh.example.test") {
		t.Fatalf("expected domain in %q", message)
	}
}

func TestDescribeNgrokStartupErrorForTimeout(t *testing.T) {
	t.Parallel()

	message := describeNgrokStartupError(context.DeadlineExceeded, "mesh.example.test")
	if !strings.Contains(message, "timed out after 30s") {
		t.Fatalf("unexpected message %q", message)
	}
}

func TestStartupInterrupted(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !startupInterrupted(ctx) {
		t.Fatal("expected canceled context to interrupt startup")
	}
}
