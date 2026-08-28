package purity

import (
	"context"
	"encoding/json"
	"net"
	"strings"

	"mihomo-guardian/internal/config"
	"mihomo-guardian/internal/probe"
)

// Fetcher owns the network transport. In production it is implemented by
// probe.ExternalClient, which is forced through mihomo's local proxy.
type Fetcher interface {
	Fetch(context.Context, config.ProbeSpec) (probe.Result, []byte)
}

func Collect(ctx context.Context, fetcher Fetcher, urls []string) []Lookup {
	if fetcher == nil {
		return nil
	}
	results := make([]Lookup, 0, len(urls))
	for _, rawURL := range urls {
		result, body := fetcher.Fetch(ctx, config.ProbeSpec{
			ID: "purity", URL: rawURL, Method: "GET", Enabled: true,
			ExpectedMin: 200, ExpectedMax: 499,
		})
		if result.Class != probe.ReachableHTTP {
			continue
		}
		if lookup, ok := parseLookup(body); ok {
			results = append(results, lookup)
		}
	}
	return results
}

func parseLookup(body []byte) (Lookup, bool) {
	trimmed := strings.TrimSpace(string(body))
	if ip := net.ParseIP(strings.Trim(trimmed, "\"")); ip != nil {
		return Lookup{IP: ip.String()}, true
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return Lookup{}, false
	}
	lookup := Lookup{
		IP:           firstString(payload, "ip", "query", "address"),
		Country:      firstString(payload, "country", "countryCode", "country_code"),
		ASN:          firstString(payload, "asn", "as", "autonomous_system"),
		Organization: firstString(payload, "organization", "org", "isp"),
		Datacenter:   firstBool(payload, "datacenter", "hosting", "host", "proxy"),
	}
	if lookup.IP == "" || net.ParseIP(lookup.IP) == nil {
		return Lookup{}, false
	}
	return lookup, true
}

func firstString(payload map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := payload[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func firstBool(payload map[string]any, keys ...string) bool {
	for _, key := range keys {
		switch value := payload[key].(type) {
		case bool:
			if value {
				return true
			}
		case string:
			if strings.EqualFold(value, "true") || strings.EqualFold(value, "hosting") || strings.EqualFold(value, "datacenter") {
				return true
			}
		}
	}
	return false
}
