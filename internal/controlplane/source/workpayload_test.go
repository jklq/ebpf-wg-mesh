package source

import (
	"testing"

	"ebof-wg-mesh/internal/controlplane/durablework"
)

func TestWorkPayloadRoundTrip(t *testing.T) {
	t.Parallel()
	want := WorkPayload{
		ServiceID:                    "svc-1",
		SpecRevision:                 7,
		Provider:                     "github",
		ProviderRepositoryExternalID: "42",
		ProviderScopeExternalID:      "9",
		TrackedRef:                   "main",
		CommitSHA:                    "abc123",
		CommitMessage:                "hello",
		CommitAuthor:                 "octocat",
	}
	raw, err := EncodeWorkPayload(want)
	if err != nil {
		t.Fatalf("EncodeWorkPayload: %v", err)
	}
	got, err := DecodeWorkPayload(raw)
	if err != nil {
		t.Fatalf("DecodeWorkPayload: %v", err)
	}
	if got != want {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
}

func TestDecodeWorkPayloadRejectsCorruptJSON(t *testing.T) {
	t.Parallel()
	if _, err := DecodeWorkPayload([]byte("{nope")); err == nil {
		t.Fatal("expected decode error")
	}
	if got, err := DecodeWorkPayload(nil); err != nil || got != (WorkPayload{}) {
		t.Fatalf("empty payload = (%+v, %v), want zero value", got, err)
	}
}

func TestSourceWorkParamBuilders(t *testing.T) {
	t.Parallel()
	spec := SourceSpecChangedParams("svc-1", 7, false)
	if spec.Kind != SourceWorkKindSourceSpecChanged || spec.DedupKey != "source_spec_changed:svc-1:7" {
		t.Fatalf("spec changed params = %+v", spec)
	}
	if spec.ResourceType != "service" || spec.ResourceID != "svc-1" {
		t.Fatalf("spec changed resource = %s/%s", spec.ResourceType, spec.ResourceID)
	}
	forced := SourceSpecChangedParams("svc-1", 7, true)
	if forced.DedupKey == spec.DedupKey {
		t.Fatal("forced key must be unique")
	}
	resync := SourceResyncParams("svc-1")
	if resync.DedupKey != "source_spec_changed:svc-1" {
		t.Fatalf("resync key = %q", resync.DedupKey)
	}
	access := ProviderAccessChangedParams(9)
	if access.DedupKey != "provider_access_changed:github:9" || access.ResourceType != "github_installation" || access.ResourceID != "9" {
		t.Fatalf("access params = %+v", access)
	}
	revision := RevisionObservedParams("42", "main", "abc123", "before1", "msg", "author")
	if revision.DedupKey != "revision_observed:github:42:main:abc123:before1" || revision.ResourceType != "github_repository" || revision.ResourceID != "42" {
		t.Fatalf("revision params = %+v", revision)
	}
	// A force-push back to the same commit carries a different "before";
	// it is a distinct transition and must not dedup into the older
	// item's stale proof.
	forcePush := RevisionObservedParams("42", "main", "abc123", "before2", "msg", "author")
	if forcePush.DedupKey == revision.DedupKey {
		t.Fatalf("force-push transition deduplicated by after SHA alone: %q", forcePush.DedupKey)
	}
	redelivery := RevisionObservedParams("42", "main", "abc123", "before1", "msg", "author")
	if redelivery.DedupKey != revision.DedupKey {
		t.Fatalf("redelivered transition = %q, want dedup with %q", redelivery.DedupKey, revision.DedupKey)
	}
	for name, params := range map[string]durablework.EnqueueParams{
		"spec": spec, "resync": resync, "access": access, "revision": revision,
	} {
		if params.AttemptLimit != SourceWorkAttemptLimit {
			t.Fatalf("%s attempt limit = %d, want %d", name, params.AttemptLimit, SourceWorkAttemptLimit)
		}
		payload, err := DecodeWorkPayload(params.Payload)
		if err != nil {
			t.Fatalf("%s payload undecodable: %v", name, err)
		}
		if payload == (WorkPayload{}) {
			t.Fatalf("%s payload is empty", name)
		}
	}
}
