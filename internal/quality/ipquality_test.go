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
		client:  client,
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
		client: client,
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
		client:  client,
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

func TestParseJSONEvidenceRejectsTrailingGarbage(t *testing.T) {
	var evidence SourceEvidence
	err := parseJSONEvidence(&evidence, []byte(`{"ip":"203.0.113.10"} trailing`))
	if err == nil {
		t.Fatal("JSON with trailing garbage must be rejected")
	}
}

func TestParseJSONEvidenceRejectsMalformedExplicitIPEvenWithOtherFields(t *testing.T) {
	var evidence SourceEvidence
	err := parseJSONEvidence(&evidence, []byte(`{"ip":"not-an-ip","country":"US"}`))
	if err == nil || !strings.Contains(err.Error(), "invalid IP") {
		t.Fatalf("error=%v, malformed explicit IP must fail closed", err)
	}
}

func TestCollectorDoesNotCountVendorWhenResponseBodyReadFails(t *testing.T) {
	const vendorURL = "https://openai.example/partial"
	client := &fakeExternalFetcher{responses: map[string][]fakeFetchResponse{
		vendorURL: {{result: probe.Result{Class: probe.ReachableHTTP, Status: 200, Err: "unexpected EOF"}, body: "partial"}},
	}}
	got, err := (&Collector{
		client:  client,
		Vendors: []VendorProbeSpec{{Vendor: "openai", URL: vendorURL}},
	}).Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	result := got.VendorResults["openai"]
	if result.SuccessCount != 0 || result.Reachable {
		t.Fatalf("vendor=%+v, body read error must not be counted as reachable", result)
	}
}

func TestCollectorDoesNotCountRoutePolicyRejectedVendor(t *testing.T) {
	const vendorURL = "https://gemini.example/models"
	client := &fakeExternalFetcher{responses: map[string][]fakeFetchResponse{
		vendorURL: {{result: probe.Result{Class: probe.RoutePolicyError, Status: 400}}},
	}}
	got, err := (&Collector{
		client:  client,
		Vendors: []VendorProbeSpec{{Vendor: "gemini", URL: vendorURL, RejectBodyPatterns: []string{`(?i)user\s+location`}}},
	}).Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	result := got.VendorResults["gemini"]
	if result.SuccessCount != 0 || result.Reachable {
		t.Fatalf("vendor=%+v, route policy rejection must not be counted as reachable", result)
	}
}

func TestParseJSONEvidencePrefersTopLevelFieldsDeterministically(t *testing.T) {
	const body = `{"meta":{"ip":"198.51.100.20","country":"CN"},"ip":"203.0.113.10","country":"US"}`
	for attempt := 0; attempt < 100; attempt++ {
		var evidence SourceEvidence
		if err := parseJSONEvidence(&evidence, []byte(body)); err != nil {
			t.Fatal(err)
		}
		if evidence.IP != "203.0.113.10" || evidence.Country != "US" {
			t.Fatalf("attempt %d evidence=%+v, want deterministic top-level fields", attempt, evidence)
		}
	}
}

func TestParseJSONEvidencePrioritizesTopLevelAliasesOverNestedFields(t *testing.T) {
	var evidence SourceEvidence
	const body = `{"details":{"ip":"198.51.100.20","country":"CN"},"ip_address":"203.0.113.10","country_code":"US"}`
	if err := parseJSONEvidence(&evidence, []byte(body)); err != nil {
		t.Fatal(err)
	}
	if evidence.IP != "203.0.113.10" || evidence.Country != "US" {
		t.Fatalf("evidence=%+v, top-level aliases must beat nested canonical fields", evidence)
	}
}

func TestParseJSONEvidenceRetainsAbuseAndBlacklistFields(t *testing.T) {
	var evidence SourceEvidence
	if err := parseJSONEvidence(&evidence, []byte(`{"ip":"203.0.113.10","abuse":true,"blacklist":true}`)); err != nil {
		t.Fatal(err)
	}
	if evidence.Abuse == nil || !*evidence.Abuse {
		t.Fatalf("abuse=%v, want true", evidence.Abuse)
	}
	if evidence.Blacklist == nil || !*evidence.Blacklist {
		t.Fatalf("blacklist=%v, want true", evidence.Blacklist)
	}
}

func TestConsensusIdentityUsesTheSameDistinctSourceRuleAsScoring(t *testing.T) {
	ip, _, complete, conflict := consensusIdentity([]SourceEvidence{
		{Source: "source-a", URL: "https://a.example/ip", Available: true, IP: "203.0.113.10"},
		{Source: "source-a", URL: "https://a.example/ip", Available: true, IP: "203.0.113.10"},
		{Source: "source-b", URL: "https://b.example/ip", Available: true, IP: "203.0.113.11"},
		{Source: "source-b", URL: "https://b.example/ip", Available: true, IP: "203.0.113.11"},
	})
	if complete || !conflict || ip == "" {
		t.Fatalf("consensus=%q complete=%v conflict=%v, duplicate sources must not manufacture consensus", ip, complete, conflict)
	}
}

func TestCollectorFailsClosedWhenSourceBodyReadReportsAnError(t *testing.T) {
	const sourceURL = "https://identity.example/partial"
	client := &fakeExternalFetcher{responses: map[string][]fakeFetchResponse{
		sourceURL: {{
			result: probe.Result{Class: probe.ReachableHTTP, Status: 200, Err: "unexpected EOF"},
			body:   "203.0.113.10",
		}},
	}}
	got, err := (&Collector{
		client:  client,
		Sources: []SourceSpec{{ID: "partial", URL: sourceURL, Kind: SourceKindIP, Format: SourceFormatText}},
	}).Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.SourceEvidence) != 1 || got.SourceEvidence[0].Available {
		t.Fatalf("source evidence=%+v, body read error must not be available", got.SourceEvidence)
	}
	if got.SourceEvidence[0].Error == nil || got.SourceEvidence[0].Error.Code != ErrorSourceUnavailable {
		t.Fatalf("source error=%+v, want source_unavailable", got.SourceEvidence[0].Error)
	}
}

func TestCollectorClassifiesContextCancellationBeforeTransportText(t *testing.T) {
	const sourceURL = "https://identity.example/canceled"
	client := &fakeExternalFetcher{responses: map[string][]fakeFetchResponse{
		sourceURL: {{result: probe.Result{Class: probe.NetworkError, Err: "lookup identity.example: context canceled"}}},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := (&Collector{
		client:  client,
		Sources: []SourceSpec{{ID: "canceled", URL: sourceURL, Kind: SourceKindIP, Format: SourceFormatText}},
	}).Collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Errors) != 1 || got.Errors[0].Code != ReportErrorCode("canceled") {
		t.Fatalf("errors=%+v, cancellation must not be classified from lookup text", got.Errors)
	}
}

func TestVendorProbesFromConfigSkipsDisabledKnownVendors(t *testing.T) {
	got := VendorProbesFromConfig([]config.ProbeSpec{
		{ID: "openai", URL: "https://openai.example/health", Enabled: false},
		{ID: "gemini", URL: "https://gemini.example/health", Enabled: true},
	})
	if len(got) != 1 || got[0].Vendor != "gemini" {
		t.Fatalf("vendor probes=%+v, disabled openai must be excluded", got)
	}
}

func TestCollectorRejectsNonProxyFetcherAtProductionBoundary(t *testing.T) {
	_, err := (&Collector{Client: &fakeExternalFetcher{}}).Collect(context.Background())
	if err == nil || !strings.Contains(err.Error(), "probe.NewExternalClient") {
		t.Fatalf("Collect error=%v, production boundary must reject non-proxied fetchers", err)
	}
}

func TestCollectEvidenceRejectsNilProxyClientWithoutPanic(t *testing.T) {
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("nil proxy client panicked: %v", recovered)
		}
	}()
	_, err := CollectEvidence(context.Background(), nil, []SourceSpec{{
		ID: "ip", URL: "https://identity.example/ip", Kind: SourceKindIP, Format: SourceFormatText,
	}}, nil)
	if err == nil {
		t.Fatal("nil proxy client must be rejected")
	}
}
