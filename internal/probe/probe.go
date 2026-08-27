package probe

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"mihomo-guardian/internal/config"
)

type ResultClass string

const (
	ReachableHTTP     ResultClass = "reachable_http"
	UpstreamHTTPError ResultClass = "upstream_http_error"
	UnexpectedHTTP    ResultClass = "unexpected_http"
	NetworkError      ResultClass = "network_error"
)

type Result struct {
	ProbeID  string
	URL      string
	Class    ResultClass
	Status   int
	Duration time.Duration
	Err      string
}

type ExternalClient struct {
	client *http.Client
}

func NewExternalClient(proxyRaw string, timeout time.Duration) (*ExternalClient, error) {
	if timeout <= 0 {
		return nil, errors.New("external probe timeout must be positive")
	}
	proxy, err := url.Parse(proxyRaw)
	if err != nil || (proxy.Scheme != "http" && proxy.Scheme != "https") || proxy.Hostname() == "" {
		return nil, errors.New("external probes require an http(s) mihomo proxy")
	}
	if !isLoopback(proxy.Hostname()) {
		return nil, errors.New("external probe proxy must point to loopback")
	}
	transport := &http.Transport{
		Proxy:                 http.ProxyURL(proxy),
		DialContext:           (&net.Dialer{Timeout: timeout}).DialContext,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		ExpectContinueTimeout: timeout,
	}
	return &ExternalClient{client: &http.Client{Transport: transport, Timeout: timeout}}, nil
}

func (c *ExternalClient) Check(ctx context.Context, spec config.ProbeSpec) Result {
	started := time.Now()
	result := Result{ProbeID: spec.ID, URL: redactURL(spec.URL)}
	method := spec.Method
	if method == "" {
		method = http.MethodGet
	}
	req, err := http.NewRequestWithContext(ctx, method, spec.URL, nil)
	if err != nil {
		result.Class = NetworkError
		result.Err = err.Error()
		result.Duration = time.Since(started)
		return result
	}
	req.Header.Set("User-Agent", "mihomo-guardian/1.0")
	resp, err := c.client.Do(req)
	result.Duration = time.Since(started)
	if err != nil {
		result.Class = classifyNetworkError(err)
		result.Err = redactError(err)
		return result
	}
	defer resp.Body.Close()
	result.Status = resp.StatusCode
	result.Class = ClassifyStatus(resp.StatusCode, spec.ExpectedMin, spec.ExpectedMax)
	return result
}

func ClassifyStatus(status, expectedMin, expectedMax int) ResultClass {
	if status == 0 {
		return NetworkError
	}
	if expectedMin == 0 {
		expectedMin = 200
	}
	if expectedMax == 0 {
		expectedMax = 499
	}
	if status >= 500 {
		return UpstreamHTTPError
	}
	if status >= expectedMin && status <= expectedMax && status >= 200 && status <= 499 {
		return ReachableHTTP
	}
	return UnexpectedHTTP
}

func classifyNetworkError(err error) ResultClass {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return NetworkError
	}
	return NetworkError
}

func redactURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "<invalid-url>"
	}
	query := parsed.Query()
	for key := range query {
		lower := strings.ToLower(key)
		if strings.Contains(lower, "token") || strings.Contains(lower, "secret") || strings.Contains(lower, "key") || strings.Contains(lower, "password") {
			query.Set(key, "<redacted>")
		}
	}
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func redactError(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("%s", err)
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
