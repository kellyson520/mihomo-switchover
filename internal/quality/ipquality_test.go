package quality

import (
	"context"
	"strings"
	"testing"
	"time"

	"mihomo-guardian/internal/config"
	"mihomo-guardian/internal/probe"
)

type fakeExternalFetcher struct {
	responses map[string][]fakeFetchResponse
	calls     []string
}

type fakeFetchResponse struct {
	result probe.Result
	body   string
}

func (f *fakeExternalFetcher) Fetch(_ context.Context, spec config.ProbeSpec) (probe.Result, []byte) {
	f.calls = append(f.calls, spec.ID+" "+spec.URL)
	items := f.responses[spec.URL]
	if len(items) == 0 {
		return probe.Result{ProbeID: spec.ID, URL: spec.URL, Class: probe.NetworkError, Err: "connection refused"}, nil
	}
	item := items[0]
	if len(items) > 1 {
		f.responses[spec.URL] = items[1:]
	}
	return item.result, []byte(item.body)
}

func TestCollectorRequiresExplicitHTTPSAndUsesOnlyInjectedClient(t *testing.T) {
	client := &fakeExternalFetcher{}
	c := Collector{
		Client:  client,
		Sources: []SourceSpec{{ID: "ip", URL: "http://insecure.example/ip", Kind: SourceKindIP, Format: SourceFormatText}},
	}
	if _, err := c.Collect(context.Background()); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("Collect error=%v, want explicit HTTPS validation", err)
	}
	if len(client.calls) != 0 {
		t.Fatalf("insecure source caused requests: %v", client.calls)
	}
}

func TestCollectorGetsIdentityRiskAndAllConfiguredVendorProbesThroughClient(t *testing.T) {
	urls := []string{
		"https://identity.example/ip-a",
		"https://identity.example/ip-b",
		"https://identity.example/risk",
	}
	client := &fakeExternalFetcher{responses: map[string][]fakeFetchResponse{
		urls[0]: {{result: probe.Result{Class: probe.ReachableHTTP, Status: 200}, body: "203.0.113.10\n"}},
		urls[1]: {{result: probe.Result{Class: probe.ReachableHTTP, Status: 200}, body: `{"ip":"203.0.113.10","asn":"AS64500","country":"US"}`}},
		urls[2]: {{result: probe.Result{Class: probe.ReachableHTTP, Status: 200}, body: `{"proxy":false,"vpn":false,"tor":false,"blacklisted":false}`}},
	}}
	for _, name := range []string{"openai", "gemini", "anthropic", "openrouter", "deepseek"} {
		client.responses["https://"+name+".example/health"] = []fakeFetchResponse{
			{result: probe.Result{Class: probe.ReachableHTTP, Status: 401}},
			{result: probe.Result{Class: probe.ReachableHTTP, Status: 429}},
		}
	}

	c := Collector{
		Client: client,
		Sources: []SourceSpec{
			{ID: "ip-a", URL: urls[0], Kind: SourceKindIP, Format: SourceFormatText, Critical: true},
			{ID: "ip-b", URL: urls[1], Kind: SourceKindIdentity, Format: SourceFormatJSON, Critical: true},
			{ID: "risk", URL: urls[2], Kind: SourceKindRisk, Format: SourceFormatJSON, Critical: true},
		},
		Vendors: []VendorProbeSpec{
			{Vendor: "openai", URL: "https://openai.example/health", Critical: true},
			{Vendor: "gemini", URL: "https://gemini.example/health", Critical: true},
			{Vendor: "anthropic", URL: "https://anthropic.example/health", Critical: true},
			{Vendor: "openrouter", URL: "https://openrouter.example/health", Critical: true},
			{Vendor: "deepseek", URL: "https://deepseek.example/health", Critical: true},
		},
	}

	got, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.VendorResults) != 5 || !got.IdentityComplete || got.IdentityIP != "203.0.113.10" {
		t.Fatalf("collection=%+v, want five vendors and complete identity", got)
	}
	for name, result := range got.VendorResults {
		if result.Attempts != 2 || !result.Reachable {
			t.Errorf("vendor %s=%+v, want two reachable attempts", name, result)
		}
	}
	for _, call := range client.calls {
		parts := strings.SplitN(call, " ", 2)
		if len(parts) != 2 || !strings.HasPrefix(parts[1], "https://") {
			t.Fatalf("unexpected call %q", call)
		}
	}
}

func TestCollectorPreservesTypedSourceErrorsAndRequiresTwoCriticalAttempts(t *testing.T) {
	const sourceURL = "https://identity.example/broken"
	const vendorURL = "https://openai.example/health"
	client := &fakeExternalFetcher{responses: map[string][]fakeFetchResponse{
		sourceURL: {{result: probe.Result{Class: probe.NetworkError, Err: "lookup identity.example: no such host"}}},
		vendorURL: {
			{result: probe.Result{Class: probe.UpstreamHTTPError, Status: 503}},
			{result: probe.Result{Class: probe.NetworkError, Err: "i/o timeout"}},
		},
	}}
	c := Collector{
		Client:  client,
		Sources: []SourceSpec{{ID: "broken", URL: sourceURL, Kind: SourceKindIP, Format: SourceFormatText, Critical: true}},
		Vendors: []VendorProbeSpec{{Vendor: "openai", URL: vendorURL, Critical: true}},
		Now:     func() time.Time { return time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC) },
	}
	got, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(client.calls) != 3 {
		t.Fatalf("calls=%v, want one source plus two vendor attempts", client.calls)
	}
	if got.VendorResults["openai"].Attempts != 2 || got.VendorResults["openai"].Reachable {
		t.Fatalf("vendor=%+v, want two failed attempts", got.VendorResults["openai"])
	}
	if len(got.Errors) < 2 || got.Errors[0].Code != ErrorDNS || got.Errors[1].Code != ErrorHTTP {
		t.Fatalf("errors=%+v, want typed DNS and HTTP errors", got.Errors)
	}
}
