package purity

import "strings"

type Lookup struct {
	IP           string
	Country      string
	ASN          string
	Organization string
	Datacenter   bool
}

type Result struct {
	IP          string
	Warning     string
	Score       int
	HardFailure bool
}

func Assess(lookups []Lookup) Result {
	if len(lookups) == 0 {
		return Result{Warning: "no_lookup_result", Score: 0}
	}
	result := Result{Score: 100}
	first := lookups[0]
	result.IP = first.IP
	for _, current := range lookups[1:] {
		if current.IP != "" && first.IP != "" && current.IP != first.IP {
			result.Warning = "lookup_conflict"
			result.Score -= 30
		}
		if current.Country != "" && first.Country != "" && current.Country != first.Country {
			result.Warning = joinWarning(result.Warning, "country_conflict")
			result.Score -= 10
		}
	}
	for _, lookup := range lookups {
		if lookup.Datacenter {
			result.Warning = joinWarning(result.Warning, "datacenter")
			result.Score -= 40
		}
		if strings.TrimSpace(lookup.ASN) == "" {
			result.Warning = joinWarning(result.Warning, "asn_unknown")
			result.Score -= 5
		}
	}
	if result.Score < 0 {
		result.Score = 0
	}
	return result
}

func joinWarning(existing, next string) string {
	if existing == "" {
		return next
	}
	if strings.Contains(existing, next) {
		return existing
	}
	return existing + "," + next
}
