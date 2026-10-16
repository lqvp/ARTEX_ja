package server

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/lqvp/artex_ja/selfupdate"
)

// releaseCache is the layer that protects the GitHub quota: the unauthenticated API allows
// only 60 requests/hour/IP, while the top-bar "new version" hint queries on every full page
// load. Once the cache fails, a few extra tabs exhaust the quota and real update checks can't reach GitHub.

func newTestCache(fetch func(context.Context, *http.Client) (*selfupdate.Release, error)) *releaseCache {
	return &releaseCache{fetch: fetch}
}

func TestReleaseCacheServesFromCache(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	for range 5 {
		rel, err := c.get(t.Context(), nil, false)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if rel.TagName != "v0.3.8" {
			t.Fatalf("TagName = %q", rel.TagName)
		}
	}
	if calls != 1 {
		t.Errorf("5 queries should hit source only once, got %d", calls)
	}
}

func TestReleaseCacheForceBypasses(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	// Clicking "check for updates" must return a live result, or a just-released version stays invisible until the cache expires.
	if _, err := c.get(t.Context(), nil, true); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("force should bypass cache, expect 2 source hits, got %d", calls)
	}
}

func TestReleaseCacheExpiresAfterTTL(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	// Shift the stored time back to just past expiry to simulate the TTL elapsing.
	c.at = time.Now().Add(-releaseTTL - time.Second)
	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("after TTL expiry should re-fetch, expect 2 source hits, got %d", calls)
	}
}

func TestReleaseCacheUsesShorterTTLForErrors(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return nil, errors.New("github unreachable")
	})

	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("expected error")
	}
	// Failures must be cached briefly too, or every page load waits out a timeout while GitHub is unreachable.
	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("expected error")
	}
	if calls != 1 {
		t.Errorf("errors should be short-cached, expect 1 source hit, got %d", calls)
	}

	// But the error TTL must be clearly shorter than the success TTL so things self-heal quickly once the network returns.
	if releaseErrTTL >= releaseTTL {
		t.Fatalf("error TTL (%v) must be shorter than success TTL (%v)", releaseErrTTL, releaseTTL)
	}
	c.at = time.Now().Add(-releaseErrTTL - time.Second)
	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("expected error")
	}
	if calls != 2 {
		t.Errorf("after error TTL expiry should retry, expect 2 source hits, got %d", calls)
	}
}

func TestReleaseCacheDoesNotPoisonOnCallerCancel(t *testing.T) {
	good := &selfupdate.Release{TagName: "v0.3.8"}
	c := newTestCache(func(ctx context.Context, _ *http.Client) (*selfupdate.Release, error) {
		return good, nil
	})
	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}

	// A visitor closing the tab cancels the request. That does not mean GitHub is broken; never
	// write "canceled" into the cache, or every visitor for the next 30 minutes gets a baffling error.
	c.fetch = func(ctx context.Context, _ *http.Client) (*selfupdate.Release, error) {
		return nil, ctx.Err()
	}
	c.at = time.Now().Add(-releaseTTL - time.Second) // expire the cache, forcing a fetch

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.get(ctx, nil, false); err == nil {
		t.Fatal("when caller canceled, error should be propagated to it")
	}

	// Key invariant: the canceled call leaves no trace — the cache holds neither the "canceled" error,
	// nor does it lose the previous good result.
	if c.err != nil {
		t.Fatalf("canceled error must not be cached, got %v", c.err)
	}
	if c.rel == nil || c.rel.TagName != "v0.3.8" {
		t.Fatalf("cache should keep previous good result, got %+v", c.rel)
	}

	// That cancellation produced no new data, so the next visitor should fetch again — and get a
	// normal result, unpolluted by the earlier cancellation.
	c.fetch = func(context.Context, *http.Client) (*selfupdate.Release, error) {
		return good, nil
	}
	rel, err := c.get(t.Context(), nil, false)
	if err != nil {
		t.Fatalf("normal request after cancellation should not error: %v", err)
	}
	if rel == nil || rel.TagName != "v0.3.8" {
		t.Fatalf("should get normal result, got %+v", rel)
	}
}
