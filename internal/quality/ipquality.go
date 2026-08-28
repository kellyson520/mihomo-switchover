package quality

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"mihomo-guardian/internal/config"
	"mihomo-guardian/internal/probe"
)

// SourceKind identifies the kind of public evidence returned by a source.
// Source URLs are supplied by the operator; this package intentionally has no
// built-in URL list and never downloads or executes a third-party script.
type SourceKind string

const (
	SourceKindIP       SourceKind = "ip"
	SourceKindIdentity SourceKind = "identity"
	SourceKindRisk     SourceKind = "risk"
)

type SourceFormat string

const (
	SourceFormatText SourceFormat = "text"
	SourceFormatJSON SourceFormat = "json"
)

// SourceSpec is an explicit HTTPS endpoint selected by the operator. Kind and
// Format are kept separate because an identity source can return either a
// small JSON document or a plain-text address.
type SourceSpec struct {
	ID       string
	URL      string
	Kind     SourceKind
	Format   SourceFormat
	Timeout  time.Duration
	Critical bool
}

// VendorProbeSpec adapts one configured vendor probe to the evidence
// collector. Critical probes always run at least twice, even when Attempts is
// omitted or set to one.
type VendorProbeSpec struct {
	ID          string
	Vendor      string
	URL         string
	Method      string
	Timeout     time.Duration
	Attempts    int
	Critical    bool
	ExpectedMin int
	ExpectedMax int
}

// ExternalFetcher is deliberately the narrow portion of probe.ExternalClient
// needed here. *probe.ExternalClient implements it, while tests can inject a
// deterministic fetcher without bypassing the production proxy boundary.
type ExternalFetcher interface {
	Fetch(context.Context, config.ProbeSpec) (probe.Result, []byte)
}

type Collection struct {
	VendorResults    map[string]VendorResult
	SourceEvidence   []SourceEvidence
	RiskEvidence     []SourceEvidence
	Errors           []ReportError
	IdentityIP       string
	IdentityFamily   string
	IdentityComplete bool
	IdentityConflict bool
}

type Collector struct {
	Client  ExternalFetcher
	Sources []SourceSpec
	Vendors []VendorProbeSpec
	Now     func() time.Time
}

func NewCollector(client *probe.ExternalClient, sources []SourceSpec, vendors []VendorProbeSpec) *Collector {
	return &Collector{Client: client, Sources: sources, Vendors: vendors}
}

// CollectEvidence is the production entry point. The concrete client must be
// created for the target loopback listener by probe.NewExternalClient before
// it is passed here.
func CollectEvidence(ctx context.Context, client *probe.ExternalClient, sources []SourceSpec, vendors []VendorProbeSpec) (Collection, error) {
	return (&Collector{Client: client, Sources: sources, Vendors: vendors}).Collect(ctx)
}

func (c *Collector) Collect(ctx context.Context) (Collection, error) {
	result := Collection{VendorResults: make(map[string]VendorResult)}
	if c == nil || c.Client == nil {
		return result, errors.New("quality collector requires an external client")
	}
	if err := c.validateEndpoints(); err != nil {
		return result, err
	}
	now := time.Now().UTC()
	if c.Now != nil {
		now = c.Now().UTC()
	}

	for _, source := range c.Sources {
		evidence, err := c.collectSource(ctx, source, now)
		if err != nil {
			if evidence.Error != nil {
				result.Errors = append(result.Errors, *evidence.Error)
			}
		}
		if source.Kind == SourceKindRisk {
			result.RiskEvidence = append(result.RiskEvidence, evidence)
		} else {
			result.SourceEvidence = append(result.SourceEvidence, evidence)
		}
	}

	for _, vendor := range c.Vendors {
		name := strings.ToLower(strings.TrimSpace(vendor.Vendor))
		if name == "" {
			name = strings.ToLower(strings.TrimSpace(vendor.ID))
		}
		if name == "" {
			result.Errors = append(result.Errors, ReportError{Code: ErrorParse, Source: vendor.ID, Message: "vendor probe has no id"})
			continue
		}
		probeResult := c.collectVendor(ctx, vendor, name, now)
		result.VendorResults[name] = probeResult
		result.Errors = append(result.Errors, probeResult.Errors...)
	}

	result.IdentityIP, result.IdentityFamily, result.IdentityComplete, result.IdentityConflict = consensusIdentity(result.SourceEvidence)
	if result.IdentityConflict {
		result.Errors = append(result.Errors, ReportError{Code: ErrorIPConflict, Source: "identity", Message: "IP sources have no two-source consensus", ObservedAt: now})
	}
	return result, nil
}

func (c *Collector) validateEndpoints() error {
	for _, source := range c.Sources {
		if err := validateHTTPSURL(source.URL); err != nil {
			return fmt.Errorf("quality source %q: %w", firstNonEmpty(source.ID, source.URL), err)
		}
	}
	for _, vendor := range c.Vendors {
		name := firstNonEmpty(vendor.Vendor, vendor.ID)
		if err := validateHTTPSURL(vendor.URL); err != nil {
			return fmt.Errorf("quality vendor %q: %w", name, err)
		}
	}
	return nil
}

func (c *Collector) collectSource(ctx context.Context, source SourceSpec, now time.Time) (SourceEvidence, error) {
	evidence := SourceEvidence{Source: strings.TrimSpace(source.ID), Kind: string(source.Kind), URL: source.URL, ObservedAt: now}
	if evidence.Source == "" {
		evidence.Source = source.URL
	}
	if err := validateHTTPSURL(source.URL); err != nil {
		return sourceFailure(evidence, ErrorSourceUnavailable, err.Error(), now)
	}
	if source.Kind == "" {
		source.Kind = SourceKindIP
		evidence.Kind = string(source.Kind)
	}
	if source.Format == "" {
		source.Format = SourceFormatText
	}

	spec := config.ProbeSpec{
		ID:          evidence.Source,
		URL:         source.URL,
		Method:      "GET",
		Timeout:     source.Timeout,
		ExpectedMin: 200,
		ExpectedMax: 499,
	}
	fetched, body := c.Client.Fetch(ctx, spec)
	evidence.HTTPStatus = fetched.Status
	evidence.LatencyMS = durationMillis(fetched.Duration)
	if fetched.Class == probe.NetworkError || fetched.Status == 0 {
		code := classifyTransportError(fetched.Err)
		return sourceFailure(evidence, code, fetched.Err, now)
	}
	if fetched.Status < 200 || fetched.Status >= 300 {
		return sourceFailure(evidence, ErrorHTTP, fmt.Sprintf("HTTP status %d", fetched.Status), now)
	}

	var err error
	switch source.Format {
	case SourceFormatText:
		err = parseTextEvidence(&evidence, body)
	case SourceFormatJSON:
		err = parseJSONEvidence(&evidence, body)
	default:
		err = fmt.Errorf("unsupported source format %q", source.Format)
	}
	if err != nil {
		return sourceFailure(evidence, ErrorParse, err.Error(), now)
	}
	evidence.Available = true
	evidence.ConfidencePercent = 100
	return evidence, nil
}

func (c *Collector) collectVendor(ctx context.Context, vendor VendorProbeSpec, name string, now time.Time) VendorResult {
	result := VendorResult{Vendor: name}
	attempts := vendor.Attempts
	if attempts < 1 {
		attempts = 1
	}
	if vendor.Critical && attempts < 2 {
		attempts = 2
	}
	result.Attempts = attempts
	result.Errors = nil

	if err := validateHTTPSURL(vendor.URL); err != nil {
		result.Errors = append(result.Errors, ReportError{Code: ErrorSourceUnavailable, Source: name, Message: err.Error(), ObservedAt: now})
		return result
	}
	for i := 0; i < attempts; i++ {
		spec := config.ProbeSpec{
			ID:          firstNonEmpty(vendor.ID, name),
			URL:         vendor.URL,
			Method:      firstNonEmpty(vendor.Method, "GET"),
			Timeout:     vendor.Timeout,
			ExpectedMin: defaultInt(vendor.ExpectedMin, 200),
			ExpectedMax: defaultInt(vendor.ExpectedMax, 499),
		}
		fetched, _ := c.Client.Fetch(ctx, spec)
		result.LastAttemptAt = now
		if fetched.Status > 0 {
			result.StatusCodes = append(result.StatusCodes, fetched.Status)
		}
		if fetched.Duration > 0 {
			result.LatencyMS = append(result.LatencyMS, durationMillis(fetched.Duration))
		}
		if isReachableVendorStatus(fetched.Status) {
			result.SuccessCount++
			continue
		}
		code := ErrorHTTP
		message := fmt.Sprintf("HTTP status %d", fetched.Status)
		if fetched.Status == 0 || fetched.Class == probe.NetworkError {
			code = classifyTransportError(fetched.Err)
			message = fetched.Err
		}
		result.Errors = append(result.Errors, ReportError{Code: code, Source: name, Message: message, ObservedAt: now})
	}
	result.Reachable = result.SuccessCount > 0
	return result
}

func isReachableVendorStatus(status int) bool { return status >= 200 && status < 500 }

func validateHTTPSURL(raw string) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil {
		return errors.New("source URL must be an explicit https URL without credentials")
	}
	return nil
}

func sourceFailure(evidence SourceEvidence, code ReportErrorCode, message string, now time.Time) (SourceEvidence, error) {
	if strings.TrimSpace(message) == "" {
		message = string(code)
	}
	evidence.Available = false
	evidence.ConfidencePercent = 0
	evidence.Error = &ReportError{Code: code, Source: evidence.Source, Message: sanitizeMessage(message), ObservedAt: now}
	return evidence, evidence.Error
}

func classifyTransportError(message string) ReportErrorCode {
	text := strings.ToLower(message)
	switch {
	case strings.Contains(text, "timeout"), strings.Contains(text, "deadline"):
		return ErrorTimeout
	case strings.Contains(text, "no such host"), strings.Contains(text, "server misbehaving"), strings.Contains(text, "lookup"):
		return ErrorDNS
	case strings.Contains(text, "tls"), strings.Contains(text, "certificate"), strings.Contains(text, "handshake"):
		return ErrorTLS
	case strings.Contains(text, "connect"), strings.Contains(text, "refused"), strings.Contains(text, "reset"), strings.Contains(text, "broken pipe"):
		return ErrorTCP
	default:
		return ErrorSourceUnavailable
	}
}

func sanitizeMessage(message string) string {
	message = strings.TrimSpace(message)
	if len(message) > 240 {
		message = message[:240]
	}
	return message
}

func durationMillis(value time.Duration) int {
	if value <= 0 {
		return 0
	}
	return int(value / time.Millisecond)
}

func defaultInt(value, fallback int) int {
	if value == 0 {
		return fallback
	}
	return value
}

func firstNonEmpty(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func parseTextEvidence(evidence *SourceEvidence, body []byte) error {
	text := strings.TrimSpace(string(body))
	if text == "" {
		return errors.New("empty response")
	}
	fields := strings.Fields(text)
	if len(fields) != 1 {
		return errors.New("plain-text response is not one IP address")
	}
	return setEvidenceIP(evidence, fields[0])
}

func parseJSONEvidence(evidence *SourceEvidence, body []byte) error {
	var document any
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.UseNumber()
	if err := decoder.Decode(&document); err != nil {
		return err
	}
	values := make(map[string]any)
	collectJSONFields(document, values)
	for _, key := range []string{"ip", "ip_address", "address", "query", "origin"} {
		if value, ok := values[key]; ok {
			if err := setEvidenceIP(evidence, fmt.Sprint(value)); err == nil {
				break
			}
		}
	}
	setStringField(values, "asn", &evidence.ASN, "as", "autonomous_system")
	setStringField(values, "organization", &evidence.Organization, "org", "isp", "company")
	setStringField(values, "country", &evidence.Country, "country_code", "region")
	setBoolField(values, &evidence.Hosting, "hosting", "host", "is_hosting", "datacenter", "data_center")
	setBoolField(values, &evidence.Proxy, "proxy", "is_proxy")
	setBoolField(values, &evidence.VPN, "vpn", "is_vpn")
	setBoolField(values, &evidence.Tor, "tor", "is_tor")
	setBoolField(values, &evidence.Blacklisted, "blacklisted", "blacklist", "is_blacklisted")
	if value, ok := firstJSONValue(values, "abuse_score", "abuse", "risk_score"); ok {
		if score, err := jsonNumber(value); err == nil {
			evidence.AbuseScore = &score
		}
	}
	if evidence.IP == "" && evidence.ASN == "" && evidence.Organization == "" && evidence.Country == "" &&
		evidence.Hosting == nil && evidence.Proxy == nil && evidence.VPN == nil && evidence.Tor == nil &&
		evidence.Blacklisted == nil && evidence.AbuseScore == nil {
		return errors.New("JSON response contains no recognized evidence")
	}
	return nil
}

func collectJSONFields(value any, fields map[string]any) {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			normalized := strings.ToLower(strings.TrimSpace(key))
			if _, exists := fields[normalized]; !exists {
				fields[normalized] = item
			}
			collectJSONFields(item, fields)
		}
	case []any:
		for _, item := range typed {
			collectJSONFields(item, fields)
		}
	}
}

func firstJSONValue(fields map[string]any, keys ...string) (any, bool) {
	for _, key := range keys {
		if value, ok := fields[key]; ok {
			return value, true
		}
	}
	return nil, false
}

func setStringField(fields map[string]any, canonical string, destination *string, alternatives ...string) {
	if value, ok := firstJSONValue(fields, append([]string{canonical}, alternatives...)...); ok {
		if text := strings.TrimSpace(fmt.Sprint(value)); text != "" && text != "<nil>" {
			*destination = text
		}
	}
}

func setBoolField(fields map[string]any, destination **bool, keys ...string) {
	value, ok := firstJSONValue(fields, keys...)
	if !ok {
		return
	}
	parsed, ok := jsonBool(value)
	if ok {
		*destination = &parsed
	}
}

func jsonBool(value any) (bool, bool) {
	switch typed := value.(type) {
	case bool:
		return typed, true
	case string:
		parsed, err := strconv.ParseBool(strings.TrimSpace(typed))
		return parsed, err == nil
	default:
		return false, false
	}
}

func jsonNumber(value any) (float64, error) {
	switch typed := value.(type) {
	case json.Number:
		return typed.Float64()
	case float64:
		return typed, nil
	case string:
		return strconv.ParseFloat(strings.TrimSpace(typed), 64)
	default:
		return 0, fmt.Errorf("not a number")
	}
}

func setEvidenceIP(evidence *SourceEvidence, raw string) error {
	ip := net.ParseIP(strings.TrimSpace(raw))
	if ip == nil {
		return fmt.Errorf("invalid IP address %q", raw)
	}
	evidence.IP = ip.String()
	if ip.To4() != nil {
		evidence.IPFamily = "ipv4"
	} else {
		evidence.IPFamily = "ipv6"
	}
	return nil
}

func consensusIdentity(sources []SourceEvidence) (ip, family string, complete, conflict bool) {
	type vote struct {
		ip     string
		family string
	}
	votes := make(map[string]vote)
	counts := make(map[string]int)
	for _, source := range sources {
		if !source.Available || net.ParseIP(source.IP) == nil {
			continue
		}
		canonical := net.ParseIP(source.IP).String()
		if _, exists := votes[canonical]; !exists {
			votes[canonical] = vote{ip: canonical, family: firstNonEmpty(source.IPFamily, ipFamily(canonical))}
		}
		counts[canonical]++
	}
	best := 0
	for candidate, count := range counts {
		if count > best {
			best = count
			ip = candidate
			family = votes[candidate].family
		}
	}
	complete = best >= 2
	conflict = len(counts) > 1 && best < 2
	return
}

func ipFamily(raw string) string {
	ip := net.ParseIP(raw)
	if ip == nil {
		return ""
	}
	if ip.To4() != nil {
		return "ipv4"
	}
	return "ipv6"
}

// VendorProbesFromConfig reuses the normal configured probe list and only
// recognizes the supported vendor IDs. Other application probes stay out of
// quality scoring unless explicitly mapped by the caller.
func VendorProbesFromConfig(probes []config.ProbeSpec) []VendorProbeSpec {
	known := map[string]string{
		"openai": "openai", "gemini": "gemini", "anthropic": "anthropic",
		"openrouter": "openrouter", "deepseek": "deepseek",
	}
	result := make([]VendorProbeSpec, 0, len(probes))
	for _, item := range probes {
		name := strings.ToLower(strings.TrimSpace(item.ID))
		vendor, ok := known[name]
		if !ok {
			continue
		}
		result = append(result, VendorProbeSpec{
			ID: item.ID, Vendor: vendor, URL: item.URL, Method: item.Method,
			Timeout: item.Timeout, Critical: item.Critical, ExpectedMin: item.ExpectedMin,
			ExpectedMax: item.ExpectedMax,
		})
	}
	return result
}
