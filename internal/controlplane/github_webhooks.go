package controlplane

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
)

type GitHubWebhookHandler struct {
	store     *Store
	secret    []byte
	processor *GitHubWebhookProcessor
}

var (
	errGitHubWebhookInvalidSignature = errors.New("invalid signature")
	errGitHubWebhookMissingHeaders   = errors.New("missing delivery headers")
)

func NewGitHubWebhookHandler(store *Store, secret string, processor *GitHubWebhookProcessor) *GitHubWebhookHandler {
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
	payload, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
	if err != nil {
		http.Error(w, "read webhook body", http.StatusBadRequest)
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
	case errors.Is(err, errGitHubWebhookInvalidSignature):
		http.Error(w, err.Error(), http.StatusUnauthorized)
	case errors.Is(err, errGitHubWebhookMissingHeaders):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		http.Error(w, "enqueue webhook", http.StatusInternalServerError)
	}
}

func (h *GitHubWebhookHandler) HandleDelivery(ctx context.Context, signature, deliveryID, eventType string, payload []byte) error {
	if !h.verifySignature(signature, payload) {
		return errGitHubWebhookInvalidSignature
	}
	deliveryID = strings.TrimSpace(deliveryID)
	eventType = strings.TrimSpace(eventType)
	if deliveryID == "" || eventType == "" {
		return errGitHubWebhookMissingHeaders
	}
	inserted, err := h.store.enqueueGitHubWebhookDelivery(ctx, deliveryID, eventType, payload)
	if err != nil {
		return err
	}
	if inserted {
		h.processor.RequestProcess()
	}
	return nil
}

func (h *GitHubWebhookHandler) verifySignature(header string, payload []byte) bool {
	signature := strings.TrimPrefix(strings.TrimSpace(header), "sha256=")
	if signature == "" {
		return false
	}
	mac := hmac.New(sha256.New, h.secret)
	_, _ = mac.Write(payload)
	expected := mac.Sum(nil)
	actual, err := hex.DecodeString(signature)
	if err != nil {
		return false
	}
	return hmac.Equal(actual, expected)
}

type GitHubWebhookProcessor struct {
	store       *Store
	coordinator *GitHubCoordinator
	requestCh   chan struct{}
	id          string
}

func NewGitHubWebhookProcessor(store *Store, coordinator *GitHubCoordinator) *GitHubWebhookProcessor {
	if store == nil || coordinator == nil || !coordinator.Enabled() {
		return nil
	}
	return &GitHubWebhookProcessor{
		store:       store,
		coordinator: coordinator,
		requestCh:   make(chan struct{}, 1),
		id:          "github-webhook-processor",
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
	p.RequestProcess()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-p.requestCh:
			for {
				rec, err := p.store.claimNextGitHubWebhookDelivery(ctx, p.id, 5*time.Minute)
				if err != nil {
					return err
				}
				if rec.ID == "" {
					break
				}
				err = p.processDelivery(ctx, rec)
				if completeErr := p.store.completeGitHubWebhookDelivery(ctx, rec.ID, err); completeErr != nil {
					return completeErr
				}
				if err != nil {
					slog.Warn("github webhook processing failed", "delivery_id", rec.DeliveryID, "event_type", rec.EventType, "error", err)
				}
			}
		}
	}
}

func (p *GitHubWebhookProcessor) processDelivery(ctx context.Context, rec githubWebhookDeliveryRecord) error {
	switch rec.EventType {
	case "push":
		return p.processPushEvent(ctx, rec.Payload)
	case "installation":
		return p.processInstallationEvent(ctx, rec.Payload)
	case "installation_repositories":
		return p.processInstallationRepositoriesEvent(ctx, rec.Payload)
	case "repository":
		return p.processRepositoryEvent(ctx, rec.Payload)
	default:
		return nil
	}
}

func (p *GitHubWebhookProcessor) processPushEvent(ctx context.Context, raw []byte) error {
	var payload struct {
		Ref          string                   `json:"ref"`
		After        string                   `json:"after"`
		Deleted      bool                     `json:"deleted"`
		Repository   webhookRepositoryPayload `json:"repository"`
		Installation struct {
			ID int64 `json:"id"`
		} `json:"installation"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return err
	}
	if payload.Deleted || strings.TrimSpace(payload.After) == "" {
		return nil
	}
	branch := strings.TrimPrefix(payload.Ref, "refs/heads/")
	if branch == payload.Ref {
		return nil
	}
	if err := p.coordinator.ObserveRepositoryRevision(
		ctx,
		fmt.Sprintf("%d", payload.Repository.ID),
		branch,
		payload.After,
	); err != nil {
		return err
	}
	return nil
}

func (p *GitHubWebhookProcessor) processInstallationEvent(ctx context.Context, raw []byte) error {
	var payload struct {
		Action       string                     `json:"action"`
		Installation webhookInstallationPayload `json:"installation"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return err
	}
	switch payload.Action {
	case "deleted":
		return p.store.deactivateGitHubInstallation(ctx, payload.Installation.ID)
	default:
		if err := p.store.upsertGitHubInstallation(ctx, githubInstallationRecord{
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
	if err := p.store.upsertGitHubInstallation(ctx, githubInstallationRecord{
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
		if err := p.store.markGitHubRepositorySnapshotDeleted(ctx, payload.Repository.Owner.Login, payload.Repository.Name); err != nil {
			return err
		}
		return p.coordinator.RequestInstallationRefresh(ctx, payload.Installation.ID)
	case "renamed", "transferred", "edited", "publicized", "privatized":
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
