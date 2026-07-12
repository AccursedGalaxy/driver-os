package headless

import "testing"

func TestStampRoleModels(t *testing.T) {
	var r resultObject
	stampRoleModels(&r, " openai/gpt-5.5 ", "", "anthropic/claude-opus-4.8")
	if r.ReviewModel == nil || *r.ReviewModel != "openai/gpt-5.5" {
		t.Errorf("review model not stamped/trimmed: %v", r.ReviewModel)
	}
	if r.PlanModel != nil {
		t.Errorf("empty plan model should stay null, got %v", *r.PlanModel)
	}
	if r.SelectModel == nil || *r.SelectModel != "anthropic/claude-opus-4.8" {
		t.Errorf("select model not stamped: %v", r.SelectModel)
	}
}
