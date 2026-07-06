package gobench

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestMigrateRoundtrip(t *testing.T) {
	files, err := filepath.Glob("testdata/instances/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 5 {
		t.Errorf("expected 5 instances, got %d", len(files))
	}

	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			inst, err := LoadInstance(path)
			if err != nil {
				t.Fatalf("LoadInstance failed: %v", err)
			}

			if err := inst.Validate(); err != nil {
				t.Fatalf("Validate failed: %v", err)
			}

			// Byte-stability
			gotBytes, err := json.MarshalIndent(inst, "", "  ")
			if err != nil {
				t.Fatalf("MarshalIndent failed: %v", err)
			}
			gotBytes = append(gotBytes, '\n')

			wantBytes, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile failed: %v", err)
			}

			if string(gotBytes) != string(wantBytes) {
				t.Errorf("byte-stability failed for %s", path)
			}

			// Spot-checks
			if inst.InstanceID == "open-policy-agent__opa-8781" {
				if len(inst.FailToPass) != 1 {
					t.Errorf("opa-8781: expected 1 F2P, got %d", len(inst.FailToPass))
				} else {
					f2p := inst.FailToPass[0]
					if f2p.Name != "TestTopDownPartialEvalNegation" {
						t.Errorf("opa-8781: expected Name TestTopDownPartialEvalNegation, got %q", f2p.Name)
					}
					if f2p.Package != "./v1/topdown" {
						t.Errorf("opa-8781: expected Package ./v1/topdown, got %q", f2p.Package)
					}
					if f2p.RunRegex != "^TestTopDownPartialEvalNegation$" {
						t.Errorf("opa-8781: expected RunRegex ^TestTopDownPartialEvalNegation$, got %q", f2p.RunRegex)
					}
				}
				if len(inst.Aliases) != 1 || inst.Aliases[0] != "opa-8781" {
					t.Errorf("opa-8781: expected Aliases [\"opa-8781\"], got %v", inst.Aliases)
				}
			}

			if inst.InstanceID == "urfave__cli-2363" {
				if len(inst.FailToPass) != 2 {
					t.Errorf("urfave-2363: expected 2 F2P, got %d", len(inst.FailToPass))
				}
			}
		})
	}
}
