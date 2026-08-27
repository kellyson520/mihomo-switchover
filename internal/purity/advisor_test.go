package purity

import "testing"

func TestPurityAssessmentProducesAdvisoryWarning(t *testing.T) {
	result := Assess([]Lookup{{
		IP:           "203.0.113.10",
		Country:      "US",
		ASN:          "AS64500",
		Organization: "Example Datacenter",
		Datacenter:   true,
	}})
	if result.Warning == "" || result.Score >= 100 {
		t.Fatalf("result=%+v", result)
	}
}

func TestPurityAssessmentTreatsConflictingLookupsAsUnknown(t *testing.T) {
	result := Assess([]Lookup{
		{IP: "203.0.113.10", Country: "US", ASN: "AS64500"},
		{IP: "203.0.113.11", Country: "JP", ASN: "AS64501"},
	})
	if result.Warning == "" || result.HardFailure {
		t.Fatalf("conflicting result=%+v", result)
	}
}
