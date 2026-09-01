package quality

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

var (
	ErrNotFound       = errors.New("quality: record not found")
	ErrInvalidNodeKey = errors.New("quality: invalid node identity")
	ErrInvalidReport  = errors.New("quality: invalid report")
	ErrInvalidStore   = errors.New("quality: invalid store root")
	ErrCorruptJSON    = errors.New("quality: corrupt JSON")
	ErrScanBusy       = errors.New("quality: another full scan is already running")
)

// CorruptFileError identifies a malformed persistence file and, when
// available, the evidence path to which it was moved.
type CorruptFileError struct {
	Path   string
	Backup string
	Cause  error
}

func (e *CorruptFileError) Error() string {
	if e.Backup == "" {
		return fmt.Sprintf("%s: %v", e.Path, e.Cause)
	}
	return fmt.Sprintf("%s: %v (preserved as %s)", e.Path, e.Cause, e.Backup)
}

func (e *CorruptFileError) Unwrap() error { return ErrCorruptJSON }

// ReportErrorCode is a stable, non-sensitive classification for an evidence
// collection or validation failure. Error details are deliberately kept
// separate from controller credentials and request URLs.
type ReportErrorCode string

const (
	ErrorSourceUnavailable ReportErrorCode = "source_unavailable"
	ErrorHTTP              ReportErrorCode = "http_error"
	ErrorDNS               ReportErrorCode = "dns_error"
	ErrorTCP               ReportErrorCode = "tcp_error"
	ErrorTLS               ReportErrorCode = "tls_error"
	ErrorTimeout           ReportErrorCode = "timeout"
	ErrorCanceled          ReportErrorCode = "canceled"
	ErrorParse             ReportErrorCode = "parse_error"
	ErrorIPConflict        ReportErrorCode = "ip_conflict"
	ErrorProviderUnhealthy ReportErrorCode = "provider_unhealthy"
	ErrorStabilityUnknown  ReportErrorCode = "stability_unknown"
)

// ReportError is the serializable form of an error attached to a report.
// Message is sanitized by Store before it is written to disk.
type ReportError struct {
	Code       ReportErrorCode `json:"code"`
	Source     string          `json:"source,omitempty"`
	Message    string          `json:"message,omitempty"`
	Temporary  bool            `json:"temporary,omitempty"`
	ObservedAt time.Time       `json:"observed_at,omitempty"`
}

func (e ReportError) Error() string {
	if e.Message == "" {
		return string(e.Code)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// NodeKey identifies one exit identity. IP is intentionally part of the key:
// when a provider node gets a new exit address it receives a new record and a
// new immutable baseline. A returning address can then find its old record.
type NodeKey struct {
	Target   string `json:"target"`
	Provider string `json:"provider"`
	Node     string `json:"node"`
	IPFamily string `json:"ip_family"`
	IP       string `json:"ip"`
}

func (k NodeKey) Validate() error {
	if strings.TrimSpace(k.Target) == "" || strings.TrimSpace(k.Provider) == "" || strings.TrimSpace(k.Node) == "" {
		return fmt.Errorf("%w: target, provider, and node are required", ErrInvalidNodeKey)
	}
	family := normalizeIPFamily(k.IPFamily)
	if family == "" {
		return fmt.Errorf("%w: ip family must be ipv4 or ipv6", ErrInvalidNodeKey)
	}
	ip := net.ParseIP(strings.TrimSpace(k.IP))
	if ip == nil {
		return fmt.Errorf("%w: invalid IP address", ErrInvalidNodeKey)
	}
	if family == "ipv4" && ip.To4() == nil {
		return fmt.Errorf("%w: IPv4 family does not match IP", ErrInvalidNodeKey)
	}
	if family == "ipv6" && ip.To4() != nil {
		return fmt.Errorf("%w: IPv6 family does not match IP", ErrInvalidNodeKey)
	}
	return nil
}

// Canonical returns the form used for persistence and identity hashing.
func (k NodeKey) Canonical() NodeKey {
	result := k
	result.Target = strings.TrimSpace(result.Target)
	result.Provider = strings.TrimSpace(result.Provider)
	result.Node = strings.TrimSpace(result.Node)
	result.IPFamily = normalizeIPFamily(result.IPFamily)
	result.IP = strings.TrimSpace(result.IP)
	if ip := net.ParseIP(result.IP); ip != nil {
		result.IP = ip.String()
		if result.IPFamily == "" {
			if ip.To4() != nil {
				result.IPFamily = "ipv4"
			} else {
				result.IPFamily = "ipv6"
			}
		}
	}
	return result
}

// Identity is a deterministic, unambiguous representation of all identity
// components. JSON object encoding avoids delimiter-collision bugs in node
// names and provider names.
func (k NodeKey) Identity() string {
	data, _ := json.Marshal(k.Canonical())
	return string(data)
}

// ID is the safe filename component for this identity.
func (k NodeKey) ID() string {
	digest := sha256.Sum256([]byte(k.Identity()))
	return hex.EncodeToString(digest[:])
}

// Hash is an explicit alias for callers that prefer the storage terminology.
func (k NodeKey) Hash() string { return k.ID() }

func normalizeIPFamily(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "4", "v4", "ipv4", "ip4":
		return "ipv4"
	case "6", "v6", "ipv6", "ip6":
		return "ipv6"
	default:
		return ""
	}
}

type VendorResult struct {
	Vendor        string        `json:"vendor"`
	Attempts      int           `json:"attempts,omitempty"`
	Reachable     bool          `json:"reachable"`
	StatusCodes   []int         `json:"status_codes,omitempty"`
	SuccessCount  int           `json:"success_count,omitempty"`
	LatencyMS     []int         `json:"latency_ms,omitempty"`
	LastAttemptAt time.Time     `json:"last_attempt_at,omitempty"`
	Errors        []ReportError `json:"errors,omitempty"`
}

// SourceEvidence contains normalized results from an explicitly configured
// IP, ASN, reputation, or risk source. Pointer booleans distinguish false
// from a source that did not provide an answer.
type SourceEvidence struct {
	Source            string       `json:"source"`
	Kind              string       `json:"kind,omitempty"`
	URL               string       `json:"url,omitempty"`
	ObservedAt        time.Time    `json:"observed_at,omitempty"`
	Available         bool         `json:"available"`
	IP                string       `json:"ip,omitempty"`
	IPFamily          string       `json:"ip_family,omitempty"`
	ASN               string       `json:"asn,omitempty"`
	Organization      string       `json:"organization,omitempty"`
	Country           string       `json:"country,omitempty"`
	Hosting           *bool        `json:"hosting,omitempty"`
	Proxy             *bool        `json:"proxy,omitempty"`
	VPN               *bool        `json:"vpn,omitempty"`
	Tor               *bool        `json:"tor,omitempty"`
	Blacklisted       *bool        `json:"blacklisted,omitempty"`
	Blacklist         *bool        `json:"blacklist,omitempty"`
	Abuse             *bool        `json:"abuse,omitempty"`
	AbuseScore        *float64     `json:"abuse_score,omitempty"`
	ConfidencePercent int          `json:"confidence_percent,omitempty"`
	HTTPStatus        int          `json:"http_status,omitempty"`
	LatencyMS         int          `json:"latency_ms,omitempty"`
	Error             *ReportError `json:"error,omitempty"`
}

type ProviderHealth struct {
	Alive          bool      `json:"alive"`
	HistoryFresh   bool      `json:"history_fresh"`
	HistorySamples int       `json:"history_samples,omitempty"`
	LastSampleAt   time.Time `json:"last_sample_at,omitempty"`
	CheckedAt      time.Time `json:"checked_at,omitempty"`
}

type Report struct {
	Identity               NodeKey                 `json:"identity"`
	ReportID               string                  `json:"report_id,omitempty"`
	ObservedAt             time.Time               `json:"observed_at"`
	VendorResults          map[string]VendorResult `json:"vendor_results,omitempty"`
	SourceEvidence         []SourceEvidence        `json:"source_evidence,omitempty"`
	RiskEvidence           []SourceEvidence        `json:"risk_evidence,omitempty"`
	Provider               ProviderHealth          `json:"provider"`
	ProviderAlive          bool                    `json:"provider_alive"`
	ProviderHistoryFresh   bool                    `json:"provider_history_fresh"`
	ProviderHistorySamples int                     `json:"provider_history_samples,omitempty"`
	ProviderLastSampleAt   time.Time               `json:"provider_last_sample_at,omitempty"`
	StabilityObservedAt    time.Time               `json:"stability_observed_at,omitempty"`
	QualityScore           int                     `json:"quality_score"`
	StabilityScore         int                     `json:"stability_score"`
	EffectiveScore         int                     `json:"effective_score"`
	ConfidencePercent      int                     `json:"confidence_percent"`
	Complete               bool                    `json:"complete"`
	Eligible               bool                    `json:"eligible"`
	Errors                 []ReportError           `json:"errors,omitempty"`
}

func (r Report) Validate() error {
	if err := r.Identity.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidReport, err)
	}
	for name, score := range map[string]int{
		"quality_score": r.QualityScore, "stability_score": r.StabilityScore,
		"effective_score": r.EffectiveScore, "confidence_percent": r.ConfidencePercent,
	} {
		if score < 0 || score > 100 {
			return fmt.Errorf("%w: %s must be between 0 and 100", ErrInvalidReport, name)
		}
	}
	return nil
}

func (r Report) BaselineEligible() bool { return r.Complete && r.Eligible }

type Baseline struct {
	Identity          NodeKey   `json:"identity"`
	Score             int       `json:"score"`
	QualityScore      int       `json:"quality_score"`
	StabilityScore    int       `json:"stability_score"`
	ConfidencePercent int       `json:"confidence_percent"`
	CreatedAt         time.Time `json:"created_at"`
	ObservedAt        time.Time `json:"observed_at"`
}

type NodeRecord struct {
	Identity    NodeKey   `json:"identity"`
	Baseline    *Baseline `json:"baseline,omitempty"`
	Latest      *Report   `json:"latest,omitempty"`
	Best        *Report   `json:"best,omitempty"`
	BestScore   int       `json:"best_score,omitempty"`
	LastGood    *Report   `json:"last_good,omitempty"`
	ReportCount int       `json:"report_count"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type StabilitySnapshot struct {
	Identity            NodeKey   `json:"identity"`
	ObservedAt          time.Time `json:"observed_at"`
	WindowStart         time.Time `json:"window_start"`
	WindowEnd           time.Time `json:"window_end"`
	Known               bool      `json:"known"`
	Fresh               bool      `json:"fresh"`
	Samples             int       `json:"samples"`
	ExpectedSamples     int       `json:"expected_samples,omitempty"`
	CoveragePercent     int       `json:"coverage_percent"`
	AliveSamples        int       `json:"alive_samples,omitempty"`
	AvailabilityPercent int       `json:"availability_percent"`
	P50MS               int       `json:"p50_ms,omitempty"`
	P95MS               int       `json:"p95_ms,omitempty"`
	MaxMS               int       `json:"max_ms,omitempty"`
	JitterMS            int       `json:"jitter_ms,omitempty"`
	StabilityScore      int       `json:"stability_score"`
	LastSampleAt        time.Time `json:"last_sample_at,omitempty"`
}

type TargetScanProgress struct {
	Target              string    `json:"target"`
	Provider            string    `json:"provider,omitempty"`
	Cursor              string    `json:"cursor,omitempty"`
	CursorIndex         int       `json:"cursor_index"`
	ProviderFingerprint string    `json:"provider_fingerprint,omitempty"`
	Attempted           int       `json:"attempted"`
	Completed           int       `json:"completed"`
	Failed              int       `json:"failed"`
	LastFullScanAt      time.Time `json:"last_full_scan_at,omitempty"`
	LastAttemptAt       time.Time `json:"last_attempt_at,omitempty"`
	LastSuccessAt       time.Time `json:"last_success_at,omitempty"`
	Complete            bool      `json:"complete"`
}

type ScanProgress struct {
	Target              string                        `json:"target,omitempty"`
	Provider            string                        `json:"provider,omitempty"`
	Cursor              string                        `json:"cursor,omitempty"`
	CursorIndex         int                           `json:"cursor_index"`
	ProviderFingerprint string                        `json:"provider_fingerprint,omitempty"`
	Attempted           int                           `json:"attempted"`
	Completed           int                           `json:"completed"`
	Failed              int                           `json:"failed"`
	LastFullScanAt      time.Time                     `json:"last_full_scan_at,omitempty"`
	LastAttemptAt       time.Time                     `json:"last_attempt_at,omitempty"`
	LastSuccessAt       time.Time                     `json:"last_success_at,omitempty"`
	Complete            bool                          `json:"complete"`
	Targets             map[string]TargetScanProgress `json:"targets,omitempty"`
	UpdatedAt           time.Time                     `json:"updated_at"`
}

type Recommendation struct {
	ID                   string    `json:"id,omitempty"`
	Target               string    `json:"target"`
	SourceGroup          string    `json:"source_group"`
	Provider             string    `json:"provider"`
	Node                 string    `json:"node"`
	Identity             NodeKey   `json:"identity"`
	ReportedAt           time.Time `json:"reported_at"`
	ExpiresAt            time.Time `json:"expires_at,omitempty"`
	EffectiveScore       int       `json:"effective_score"`
	BaselineScore        int       `json:"baseline_score"`
	QualityScore         int       `json:"quality_score,omitempty"`
	StabilityScore       int       `json:"stability_score,omitempty"`
	ConfidencePercent    int       `json:"confidence_percent"`
	Complete             bool      `json:"complete"`
	Connected            bool      `json:"connected"`
	ProviderAlive        bool      `json:"provider_alive"`
	ProviderHistoryFresh bool      `json:"provider_history_fresh"`
	Reason               string    `json:"reason,omitempty"`
	CreatedAt            time.Time `json:"created_at"`
}
