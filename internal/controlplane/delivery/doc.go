// Package delivery owns deployment lifecycle, rollouts, allocation replacement,
// and placement, including their SQL and transaction-local policy helpers.
//
// Delivery exposes complete operations. Its persistence is private; other
// modules read through ReadModel and cannot invoke deployment transitions,
// allocation insertion, or rollout advancement helpers. The shared transaction
// runner preserves lease fencing, retries, and durable event-index updates.
// Notifications and ingress publication follow the commit.
package delivery
