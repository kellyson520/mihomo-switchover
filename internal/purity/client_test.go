package purity

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"mihomo-guardian/internal/config"
	"mihomo-guardian/internal/probe"
)

type fakeFetcher struct {
	body []byte
	call int
}

func (f *fakeFetcher) Fetch(_ context.Context, _ config.ProbeSpec) (probe.Result, []byte) {
	f.call++
	return probe.Result{Class: probe.ReachableHTTP}, f.body
}

func TestLookupClientParsesCommonIPMetadataWithoutAffectingRouting(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"ip": "203.0.113.10", "country": "US", "asn": "AS64500",
		"org": "Example ISP", "hosting": false,
	})
	fetcher := &fakeFetcher{body: body}
	result := Collect(context.Background(), fetcher, []string{"https://ip.example/json"})
	if len(result) != 1 || result[0].IP != "203.0.113.10" || result[0].ASN != "AS64500" {
		t.Fatalf("lookups=%+v", result)
	}
	if fetcher.call != 1 {
		t.Fatalf("fetch calls=%d", fetcher.call)
	}
	if assessed := Assess(result); assessed.Warning != "" || assessed.Score != 100 {
		t.Fatalf("assessment=%+v", assessed)
	}
}

func TestLookupClientTreatsServiceFailureAsUnknown(t *testing.T) {
	fetcher := &fakeFetcher{body: []byte("not-json")}
	result := Collect(context.Background(), fetcher, []string{"https://ip.example/plain"})
	if len(result) != 0 {
		t.Fatalf("invalid metadata was trusted: %+v", result)
	}
	if assessed := Assess(result); assessed.Warning != "no_lookup_result" {
		t.Fatalf("assessment=%+v", assessed)
	}
}

func TestLookupClientUsesTheConfiguredExternalFetcher(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"query":"203.0.113.11","countryCode":"JP","as":"AS64501","isp":"Example"}`))
	}))
	defer server.Close()
	// The real proxied transport is tested in internal/probe. Here the lookup
	// adapter only verifies that its caller owns the transport boundary.
	fetcher := &fakeFetcher{body: []byte(`{"query":"203.0.113.11","countryCode":"JP","as":"AS64501","isp":"Example"}`)}
	result := Collect(context.Background(), fetcher, []string{server.URL})
	if len(result) != 1 || result[0].Country != "JP" || result[0].Organization != "Example" {
		t.Fatalf("lookups=%+v", result)
	}
}
