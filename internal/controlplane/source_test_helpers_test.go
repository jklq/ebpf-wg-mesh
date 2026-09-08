package controlplane

import (
	routingcore "ebof-wg-mesh/internal/controlplane/routing"
	sourcecore "ebof-wg-mesh/internal/controlplane/source"
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
