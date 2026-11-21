package workshop

import (
	"testing"
	"time"

	"toolkit/internal/runpod"
)

func TestParseBudget(t *testing.T) {
	cases := []struct {
		input    string
		duration time.Duration // at $0.50/h
		display  string
	}{
		{"2h", 2 * time.Hour, "2h00"},
		{"90m", 90 * time.Minute, "1h30"},
		{"$3", 6 * time.Hour, "$3.00"},
		{"1.5$", 3 * time.Hour, "$1.50"},
		{"2usd", 4 * time.Hour, "$2.00"},
	}
	for _, c := range cases {
		budget, err := ParseBudget(c.input)
		if err != nil {
			t.Errorf("ParseBudget(%q): %v", c.input, err)
			continue
		}
		if got := budget.Duration(0.50); got != c.duration {
			t.Errorf("ParseBudget(%q).Duration = %s, want %s", c.input, got, c.duration)
		}
		if got := budget.String(); got != c.display {
			t.Errorf("ParseBudget(%q).String = %q, want %q", c.input, got, c.display)
		}
	}

	for _, invalid := range []string{"", "$", "$-1", "0h", "abc", "5 dollars"} {
		if _, err := ParseBudget(invalid); err == nil {
			t.Errorf("ParseBudget(%q) should fail", invalid)
		}
	}
}

func TestMatchGPU(t *testing.T) {
	gpu := func(id, name string, secure float64, availability string) runpod.GPUType {
		return runpod.GPUType{ID: id, Name: name, Secure: true, Price: runpod.GPUPrice{Secure: secure}, Availability: availability}
	}
	catalog := []runpod.GPUType{
		gpu("NVIDIA GeForce RTX 4090", "RTX 4090", 0.69, "HIGH"),
		gpu("NVIDIA GeForce RTX 3090", "RTX 3090", 0.46, "NONE"),
		gpu("NVIDIA RTX A4000", "RTX A4000", 0.25, "LOW"),
		gpu("NVIDIA A100 80GB PCIe", "A100 PCIe", 1.64, "MEDIUM"),
		gpu("NVIDIA A100-SXM4-80GB", "A100 SXM", 1.94, "LOW"),
	}

	cases := []struct {
		wanted   string
		maxPrice float64
		wantID   string // empty means an error is expected
	}{
		{"RTX 4090", 1.00, "NVIDIA GeForce RTX 4090"},
		{"nvidia geforce rtx 4090", 1.00, "NVIDIA GeForce RTX 4090"},
		{"4090", 1.00, "NVIDIA GeForce RTX 4090"},
		{"A4000", 1.00, "NVIDIA RTX A4000"},
		{"RTX 4090", 0.50, ""}, // over max price
		{"RTX 3090", 1.00, ""}, // not available
		{"A100", 5.00, ""},     // ambiguous
		{"H200", 5.00, ""},     // unknown
	}
	for _, c := range cases {
		got, err := MatchGPU(catalog, c.wanted, "SECURE", c.maxPrice)
		if c.wantID == "" {
			if err == nil {
				t.Errorf("MatchGPU(%q, max $%.2f) = %s, want an error", c.wanted, c.maxPrice, got.ID)
			}
			continue
		}
		if err != nil || got.ID != c.wantID {
			t.Errorf("MatchGPU(%q, max $%.2f) = %q, %v; want %q", c.wanted, c.maxPrice, got.ID, err, c.wantID)
		}
	}
}
