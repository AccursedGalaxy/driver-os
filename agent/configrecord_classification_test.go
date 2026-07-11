package agent

import (
	"reflect"
	"strings"
	"testing"

	"github.com/AccursedGalaxy/driver-os/llm"
)

func TestConfigFieldClassificationsComplete(t *testing.T) {
	if problems := checkConfigFieldClassifications(reflect.TypeOf(Config{}), configFieldClasses); len(problems) != 0 {
		t.Fatalf("Config recording classification is incomplete:\n%s", strings.Join(problems, "\n"))
	}
}

func TestConfigFieldClassificationOracleRejectsUnclassifiedField(t *testing.T) {
	type futureConfig struct {
		Known           string
		NewBehaviorKnob bool
	}
	problems := checkConfigFieldClassifications(reflect.TypeOf(futureConfig{}), map[string]configFieldClass{
		"Known": {class: excludedContent},
	})
	if len(problems) != 1 || !strings.Contains(problems[0], "NewBehaviorKnob") {
		t.Fatalf("oracle must identify an unclassified exported field, got %v", problems)
	}
}

func TestConfigRecordRuntimeIdentitiesAffectConfigHash(t *testing.T) {
	// v9: ConfigSHA256 is the CANONICAL POLICY hash (§7.3) — runtime bindings
	// like ModelInfo are recorded in Bindings but must NOT perturb policy
	// identity (two runs with the same effective policy share the hash).
	base := newConfigRecordT(Config{}, "system", nil)
	withModelInfo := newConfigRecordT(Config{ModelInfo: llm.ModelInfo{ContextWindow: 42}}, "system", nil)
	if base.ConfigSHA256 != withModelInfo.ConfigSHA256 {
		t.Fatal("runtime bindings must not perturb the canonical policy hash")
	}
	if withModelInfo.Bindings.ModelInfo.ContextWindow != 42 {
		t.Fatal("ModelInfo must be recorded in Bindings")
	}
	// A POLICY difference must change the hash.
	withPolicy := newConfigRecordT(Config{MaxIterations: 17}, "system", nil)
	if base.ConfigSHA256 == withPolicy.ConfigSHA256 {
		t.Fatal("a policy difference must change ConfigSHA256")
	}
}
