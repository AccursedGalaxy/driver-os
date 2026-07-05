package openaicompat

import (
	"testing"
)

func TestResolveUsageCost(t *testing.T) {
	tests := []struct {
		name           string
		costRaw        string
		costDetailsRaw string
		want           float64
	}{
		{
			name:           "top-level cost present and > 0",
			costRaw:        "0.5",
			costDetailsRaw: `{"upstream_inference_cost": 0.000205}`,
			want:           0.5,
		},
		{
			name:           "top-level cost 0 with cost_details.upstream_inference_cost > 0",
			costRaw:        "0",
			costDetailsRaw: `{"upstream_inference_cost": 0.000205, "upstream_inference_prompt_cost": 5.5e-05, "upstream_inference_completions_cost": 0.00015}`,
			want:           0.000205,
		},
		{
			name:           "both zero",
			costRaw:        "0",
			costDetailsRaw: `{"upstream_inference_cost": 0}`,
			want:           0,
		},
		{
			name:           "both absent/null",
			costRaw:        "null",
			costDetailsRaw: "null",
			want:           0,
		},
		{
			name:           "empty strings",
			costRaw:        "",
			costDetailsRaw: "",
			want:           0,
		},
		{
			name:           "invalid json in details",
			costRaw:        "0",
			costDetailsRaw: `{"upstream_inference_cost": "not-a-float"}`,
			want:           0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveUsageCost(tt.costRaw, tt.costDetailsRaw)
			if got != tt.want {
				t.Errorf("resolveUsageCost() = %v, want %v", got, tt.want)
			}
		})
	}
}
