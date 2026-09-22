package source

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

type GitHubWebhookHandler struct {
	store     Store
	secret    []byte
	processor interface{ RequestProcess() }
}

const MaxWebhookPayloadBytes = 2 << 20

var (
	ErrGitHubWebhookInvalidSignature = errors.New("invalid signature")
	ErrGitHubWebhookMissingHeaders   = errors.New("missing delivery headers")
	ErrGitHubWebhookPayloadTooLarge  = errors.New("webhook payload too large")
)

func NewGitHubWebhookHandler(store Store, secret string, processor interface{ RequestProcess() }) *GitHubWebhookHandler {
	if store == nil || strings.TrimSpace(secret) == "" || processor == nil {
		return nil
	}
	return &GitHubWebhookHandler{
		store:     store,
		secret:    []byte(secret),
		processor: processor,
	}
}

func (h *GitHubWebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	payload, err := io.ReadAll(io.LimitReader(r.Body, MaxWebhookPayloadBytes+1))
	if err != nil {
		http.Error(w, "read webhook body", http.StatusBadRequest)
		return
	}
	if len(payload) > MaxWebhookPayloadBytes {
		http.Error(w, ErrGitHubWebhookPayloadTooLarge.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	err = h.HandleDelivery(
		r.Context(),
		r.Header.Get("X-Hub-Signature-256"),
		r.Header.Get("X-GitHub-Delivery"),
		r.Header.Get("X-GitHub-Event"),
		payload,
	)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusAccepted)
	case errors.Is(err, ErrGitHubWebhookInvalidSignature):
		http.Error(w, err.Error(), http.StatusUnauthorized)
	case errors.Is(err, ErrGitHubWebhookMissingHeaders):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, ErrGitHubWebhookPayloadTooLarge):
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
	default:
		http.Error(w, "enqueue webhook", http.StatusInternalServerError)
	}
}

func (h *GitHubWebhookHandler) HandleDelivery(ctx context.Context, signature, deliveryID, eventType string, payload []byte) error {
	if len(payload) > MaxWebhookPayloadBytes {
		return ErrGitHubWebhookPayloadTooLarge
	}
	deliveryID = strings.TrimSpace(deliveryID)
	eventType = strings.TrimSpace(eventType)
	payloadBytes := len(payload)
	valid, details := h.verifySignature(signature, payload)
	if !valid {
		slog.Warn(
			"github webhook delivery rejected",
			"delivery_id", deliveryID,
			"event_type", eventType,
			"payload_bytes", payloadBytes,
			"reason", details.reason,
			"signature_present", details.signaturePresent,
			"signature_prefix", details.signaturePrefix,
			"expected_prefix", details.expectedPrefix,
			"secret_fingerprint", details.secretFingerprint,
		)
		return ErrGitHubWebhookInvalidSignature
	}
	if deliveryID == "" || eventType == "" {
		slog.Warn("github webhook delivery rejected", "delivery_id", deliveryID, "event_type", eventType, "payload_bytes", payloadBytes, "reason", "missing_delivery_headers")
		return ErrGitHubWebhookMissingHeaders
	}
	inserted, err := h.store.EnqueueGitHubWebhookDelivery(ctx, deliveryID, eventType, payload)
	if err != nil {
		return err
	}
	if inserted {
		slog.Info("github webhook delivery queued", "delivery_id", deliveryID, "event_type", eventType, "payload_bytes", payloadBytes)
		h.processor.RequestProcess()
	} else {
		slog.Info("github webhook delivery deduplicated", "delivery_id", deliveryID, "event_type", eventType, "payload_bytes", payloadBytes)
	}
	return nil
}

type gitHubWebhookSignatureCheck struct {
	reason            string
	signaturePresent  bool
	signaturePrefix   string
	expectedPrefix    string
	secretFingerprint string
}

func (h *GitHubWebhookHandler) verifySignature(header string, payload []byte) (bool, gitHubWebhookSignatureCheck) {
	secretHash := sha256.Sum256(h.secret)
	details := gitHubWebhookSignatureCheck{
		reason:            "invalid_signature",
		secretFingerprint: hexPrefix(secretHash[:], 12),
	}
	signature := strings.TrimPrefix(strings.TrimSpace(header), "sha256=")
	if signature == "" {
		details.reason = "missing_signature"
		return false, details
	}
	details.signaturePresent = true
	details.signaturePrefix = textPrefix(signature, 12)
	mac := hmac.New(sha256.New, h.secret)
	_, _ = mac.Write(payload)
	expected := mac.Sum(nil)
	details.expectedPrefix = hexPrefix(expected, 12)
	actual, err := hex.DecodeString(signature)
	if err != nil {
		details.reason = "malformed_signature"
		return false, details
	}
	if !hmac.Equal(actual, expected) {
		return false, details
	}
	return true, details
}

func hexPrefix(data []byte, chars int) string {
	value := hex.EncodeToString(data)
	return textPrefix(value, chars)
}

func textPrefix(value string, chars int) string {
	value = strings.TrimSpace(value)
	if chars <= 0 || len(value) <= chars {
		return value
	}
	return value[:chars]
}

type GitHubWebhookProcessor struct {
	store       Store
	coordinator *GitHubCoordinator
	requestCh   chan struct{}
	id          string
}

func NewGitHubWebhookProcessor(store Store, coordinator *GitHubCoordinator) *GitHubWebhookProcessor {
	if store == nil || coordinator == nil || !coordinator.Enabled() {
		return nil
	}
	return &GitHubWebhookProcessor{
		store:       store,
		coordinator: coordinator,
		requestCh:   make(chan struct{}, 1),
		id:          "github-webhook-processor-" + uuid.NewString(),
	}
}

func (p *GitHubWebhookProcessor) RequestProcess() {
	if p == nil {
		return
	}
	select {
	case p.requestCh <- struct{}{}:
	default:
	}
}

func (p *GitHubWebhookProcessor) Run(ctx context.Context) error {
	if p == nil {
		return nil
	}
	processPending := func() (bool, error) {
		processed := false
		for {
			rec, err := p.store.ClaimNextGitHubWebhookDelivery(ctx, p.id)
			if err != nil {
				return processed, err
			}
			if rec.ID == "" {
				return processed, nil
			}
			processed = true
			slog.InfoContext(ctx, "github webhook delivery claimed", "delivery_id", rec.DeliveryID, "event_type", rec.EventType, "processor_id", rec.ProcessorID)
			err = p.processDelivery(ctx, rec)
			if completeErr := p.store.CompleteGitHubWebhookDelivery(ctx, rec.ID, p.id, err); completeErr != nil {
				return processed, completeErr
			}
			if err != nil {
				slog.Warn("github webhook processing failed", "delivery_id", rec.DeliveryID, "event_type", rec.EventType, "error", err)
			} else {
				slog.Info("github webhook delivery processed", "delivery_id", rec.DeliveryID, "event_type", rec.EventType)
			}
		}
	}
	idleDelay := time.Second
	for {
		timer := time.NewTimer(jitter(idleDelay))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-p.requestCh:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
		}
		processed, err := processPending()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			slog.Warn("github webhook processor pass failed", "error", err)
			if !waitContext(ctx, jitter(time.Second)) {
				return nil
			}
			continue
		}
		if processed {
			idleDelay = time.Second
		} else {
			idleDelay *= 2
			if idleDelay > 30*time.Second {
				idleDelay = 30 * time.Second
			}
		}
	}
}

func (p *GitHubWebhookProcessor) processDelivery(ctx context.Context, rec GitHubWebhookDeliveryRecord) error {
	switch rec.EventType {
	case "push":
		return p.ProcessPushEvent(ctx, rec.Payload)
	case "installation":
		return p.ProcessInstallationEvent(ctx, rec.Payload)
	case "installation_repositories":
		return p.processInstallationRepositoriesEvent(ctx, rec.Payload)
	case "repository":
		return p.processRepositoryEvent(ctx, rec.Payload)
	default:
		slog.InfoContext(ctx, "github webhook event ignored", "event_type", rec.EventType, "delivery_id", rec.DeliveryID)
		return nil
	}
}

func (p *GitHubWebhookProcessor) ProcessPushEvent(ctx context.Context, raw []byte) error {
	var payload struct {
		Ref        string `json:"ref"`
		After      string `json:"after"`
		Before     string `json:"before"`
		Deleted    bool   `json:"deleted"`
		HeadCommit struct {
			Message string `json:"message"`
			Author  struct {
				Name string `json:"name"`
			} `json:"author"`
			Committer struct {
				Name string `json:"name"`
			} `json:"committer"`
		} `json:"head_commit"`
		Repository   webhookRepositoryPayload `json:"repository"`
		Installation struct {
			ID int64 `json:"id"`
		} `json:"installation"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return err
	}
	if payload.Deleted || strings.TrimSpace(payload.After) == "" {
		slog.InfoContext(ctx, "github push ignored", "repository_id", payload.Repository.ID, "repository_full_name", payload.Repository.FullName, "ref", payload.Ref, "commit_sha", payload.After, "reason", "deleted_or_empty_commit")
		return nil
	}
	branch := strings.TrimPrefix(payload.Ref, "refs/heads/")
	if branch == payload.Ref {
		slog.InfoContext(ctx, "github push ignored", "repository_id", payload.Repository.ID, "repository_full_name", payload.Repository.FullName, "ref", payload.Ref, "commit_sha", payload.After, "reason", "non_branch_ref")
		return nil
	}
	slog.InfoContext(ctx, "github push observed", "repository_id", payload.Repository.ID, "repository_full_name", payload.Repository.FullName, "tracked_ref", branch, "commit_sha", payload.After, "installation_id", payload.Installation.ID)
	commitAuthor := strings.TrimSpace(payload.HeadCommit.Author.Name)
	if commitAuthor == "" {
		commitAuthor = strings.TrimSpace(payload.HeadCommit.Committer.Name)
	}
	if err := p.coordinator.ObserveRepositoryRevision(
		ctx,
		fmt.Sprintf("%d", payload.Repository.ID),
		branch,
		payload.After,
		payload.Before,
		strings.TrimSpace(payload.HeadCommit.Message),
		commitAuthor,
	); err != nil {
		return err
	}
	return nil
}

func (p *GitHubWebhookProcessor) ProcessInstallationEvent(ctx context.Context, raw []byte) error {
	var payload struct {
		Action       string                     `json:"action"`
		Installation webhookInstallationPayload `json:"installation"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return err
	}
	switch payload.Action {
	case "deleted":
		slog.InfoContext(ctx, "github installation deleted", "installation_id", payload.Installation.ID)
		return p.store.DeactivateGitHubInstallation(ctx, payload.Installation.ID)
	default:
		slog.InfoContext(ctx, "github installation refreshed", "installation_id", payload.Installation.ID, "action", payload.Action)
		if err := p.store.UpsertGitHubInstallation(ctx, GitHubInstallationRecord{
			InstallationID: payload.Installation.ID,
			AccountLogin:   payload.Installation.Account.Login,
			AccountType:    payload.Installation.Account.Type,
			TargetType:     payload.Installation.TargetType,
			Active:         true,
		}); err != nil {
			return err
		}
		return p.coordinator.RequestInstallationRefresh(ctx, payload.Installation.ID)
	}
}

func (p *GitHubWebhookProcessor) processInstallationRepositoriesEvent(ctx context.Context, raw []byte) error {
	var payload struct {
		Action       string                     `json:"action"`
		Installation webhookInstallationPayload `json:"installation"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return err
	}
	if payload.Action != "added" && payload.Action != "removed" {
		return nil
	}
	if err := p.store.UpsertGitHubInstallation(ctx, GitHubInstallationRecord{
		InstallationID: payload.Installation.ID,
		AccountLogin:   payload.Installation.Account.Login,
		AccountType:    payload.Installation.Account.Type,
		TargetType:     payload.Installation.TargetType,
		Active:         true,
	}); err != nil {
		return err
	}
	return p.coordinator.RequestInstallationRefresh(ctx, payload.Installation.ID)
}

func (p *GitHubWebhookProcessor) processRepositoryEvent(ctx context.Context, raw []byte) error {
	var payload struct {
		Action       string                     `json:"action"`
		Repository   webhookRepositoryPayload   `json:"repository"`
		Installation webhookInstallationPayload `json:"installation"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return err
	}
	if payload.Installation.ID <= 0 {
		return nil
	}
	switch payload.Action {
	case "deleted":
		slog.InfoContext(ctx, "github repository deleted", "installation_id", payload.Installation.ID, "repository", githubFullName(payload.Repository.Owner.Login, payload.Repository.Name))
		if err := p.store.MarkGitHubRepositorySnapshotDeleted(ctx, payload.Repository.Owner.Login, payload.Repository.Name); err != nil {
			return err
		}
		return p.coordinator.RequestInstallationRefresh(ctx, payload.Installation.ID)
	case "renamed", "transferred", "edited", "publicized", "privatized":
		slog.InfoContext(ctx, "github repository refreshed", "installation_id", payload.Installation.ID, "repository", githubFullName(payload.Repository.Owner.Login, payload.Repository.Name), "action", payload.Action)
		return p.coordinator.RequestInstallationRefresh(ctx, payload.Installation.ID)
	default:
		_ = payload.Repository
		return nil
	}
}

type webhookInstallationPayload struct {
	ID         int64  `json:"id"`
	TargetType string `json:"target_type"`
	Account    struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"account"`
}

type webhookRepositoryPayload struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch"`
	Owner         struct {
		Login string `json:"login"`
	} `json:"owner"`
}
