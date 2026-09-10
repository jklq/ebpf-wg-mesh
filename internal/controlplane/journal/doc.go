// Package journal owns the committed prefix and its in-memory durable state.
// A transaction locks the cluster head before planning, commits its product
// writes and typed command together, and applies only entries read back from
// the committed journal. Deployment history is written by the product
// transaction; replay has no database or external side effects.
package journal
