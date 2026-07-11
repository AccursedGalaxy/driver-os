package agent

import (
	"testing"
	"time"
)

func TestConfigValidate(t *testing.T) {
	valid := func() Config {
		return Config{Model: failIfCalled{t}, Sandbox: sbWith(t, nil), Task: "test"}
	}
	tests := []struct {
		name string
		edit func(*Config)
	}{
		{"nil model", func(c *Config) { c.Model = nil }},
		{"nil sandbox", func(c *Config) { c.Sandbox = nil }},
		{"unknown review policy", func(c *Config) { c.ReviewPolicy = ReviewPolicy(99) }},
		{"required review without reviewer", func(c *Config) { c.ReviewPolicy = ReviewPolicyRequired }},
		{"negative iterations", func(c *Config) { c.MaxIterations = -1 }},
		{"negative tokens", func(c *Config) { c.MaxTokens = -1 }},
		{"negative run timeout", func(c *Config) { c.RunTimeout = -time.Second }},
		{"negative verify timeout", func(c *Config) { c.VerifyTimeout = -time.Second }},
		{"negative wall clock", func(c *Config) { c.MaxWallClock = -time.Second }},
		{"negative total tokens", func(c *Config) { c.MaxTotalTokens = -1 }},
		{"negative cost", func(c *Config) { c.MaxTotalCostUSD = -1 }},
		{"negative review rounds", func(c *Config) { c.ReviewRounds = -1 }},
		{"negative finish window", func(c *Config) { c.FinishNudgeWindow = -1 }},
		{"negative navigation window", func(c *Config) { c.NavSpiralWindow = -1 }},
		{"negative answer window", func(c *Config) { c.AnswerNudgeWindow = -1 }},
		{"negative churn threshold", func(c *Config) { c.ChurnNudgeRuns = -1 }},
		{"negative read window", func(c *Config) { c.ReadWindow = -1 }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid()
			tc.edit(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("Validate succeeded, want setup error")
			}
		})
	}
	if err := (Config{}).Validate(); err == nil {
		t.Fatal("zero Config validated successfully")
	}
	if err := valid().Validate(); err != nil {
		t.Fatalf("valid Config: %v", err)
	}
}

func TestReviewPolicyFromReachableCombinations(t *testing.T) {
	tests := []struct {
		required, failOpen, optional bool
		want                         ReviewPolicy
		gateRequired                 bool
	}{
		{false, false, false, ReviewPolicyDefault, true},
		{true, false, false, ReviewPolicyRequired, true},
		{false, true, false, ReviewPolicyFailOpen, false},
		{false, false, true, ReviewPolicyOptional, true},
		{true, false, true, ReviewPolicyRequiredOptional, true},
		{false, true, true, ReviewPolicyFailOpenOptional, false},
	}
	for _, tc := range tests {
		got, err := ReviewPolicyFrom(tc.required, tc.failOpen, tc.optional)
		if err != nil || got != tc.want {
			t.Fatalf("ReviewPolicyFrom(%v,%v,%v) = %v, %v; want %v", tc.required, tc.failOpen, tc.optional, got, err, tc.want)
		}
		if required := effectiveReviewRequiredT(Config{Reviewer: &errReviewer{}, ReviewPolicy: got}); required != tc.gateRequired {
			t.Errorf("policy %v gate required = %v, want %v", got, required, tc.gateRequired)
		}
		if got.optional() != tc.optional {
			t.Errorf("policy %v optional = %v, want %v", got, got.optional(), tc.optional)
		}
	}
	if _, err := ReviewPolicyFrom(true, true, false); err == nil {
		t.Fatal("contradictory required+fail-open combination accepted")
	}
}
