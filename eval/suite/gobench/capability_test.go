package gobench

import (
	"reflect"
	"testing"
)

func TestToolchainEnv(t *testing.T) {
	tests := []struct {
		goVersion string
		want      []string
	}{
		{"1.25.7", []string{"GOTOOLCHAIN=go1.25.7"}},
		{"go1.25.7", []string{"GOTOOLCHAIN=go1.25.7"}},
		{"", nil},
	}
	for _, tt := range tests {
		got := ToolchainEnv(tt.goVersion)
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("ToolchainEnv(%q) = %v, want %v", tt.goVersion, got, tt.want)
		}
	}
}

func TestP2PArgs(t *testing.T) {
	spec := ExecSpec{
		Argv:      []string{"go", "test"},
		BuildTags: []string{"integration"},
	}
	tests := []struct {
		name     string
		pkg      string
		runRegex string
		want     []string
	}{
		{
			name:     "no regex",
			pkg:      "./pkg/foo",
			runRegex: "",
			want:     []string{"go", "test", "-tags=integration", "-count=1", "./pkg/foo"},
		},
		{
			name:     "with regex",
			pkg:      "./pkg/foo",
			runRegex: "TestSomething",
			want:     []string{"go", "test", "-tags=integration", "-count=1", "-run", "TestSomething", "./pkg/foo"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := p2pArgs(spec, tt.pkg, tt.runRegex)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("p2pArgs() = %v, want %v", got, tt.want)
			}
		})
	}
}
