package controlplane

import (
	"context"
	"errors"
	"testing"
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
