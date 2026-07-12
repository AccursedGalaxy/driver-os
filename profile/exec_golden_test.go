package profile

import "testing"

// These are compatibility identities recorded in transcripts. Never update a
// literal here: ship a new versioned profile name instead.
func TestExecProfileGoldenHashes(t *testing.T) {
	want := map[string]string{
		"coding-v1":      "ec4701bde00e813f3a423f690ac639acc5d1df03e0a5bda7946be06ee2346a83",
		"interactive-v1": "17352f9226a708cbc581c640f6c0a9aeef05a7309a1883f13bb94cc1addd961a",
		"coding-v2":      "54e57cb28d9076a59bb9257f0fe4a8c105fc89bfa3d382ae084f001eeecf7028",
		"interactive-v2": "027ae05542bf42302b8c88a9400293ba78613e9563c28deb7db8ffb246214f01",
		"observe-v1":     "3d99258cfe42e9e8cc305d6f564af932fdd5ee7e0e0a80934e0c1a9fed840c54",
		"eval-swe-v1":    "70f76d929c4916c356ba8e6f0ba0e6be65d865a31839564e06b28b91d127e710", // re-pinned pre-release same-session: descriptor corrected to cmd/eval's real flag defaults before ANY transcript recorded this name; never move again
	}
	for name, hash := range want {
		e, err := ExecByName(name)
		if err != nil {
			t.Fatal(err)
		}
		if got := e.Hash(); got != hash {
			t.Errorf("%s hash changed: got %s, want %s; ship a new version name rather than editing an existing descriptor", name, got, hash)
		}
	}
}
func TestV2TablesAreComplete(t *testing.T) {
	known := map[FieldID]bool{}
	for _, f := range ProfileFields {
		known[f] = true
	}
	for name, m := range declarations {
		if len(m) != len(ProfileFields) {
			t.Errorf("%s has %d declarations, want %d", name, len(m), len(ProfileFields))
		}
		for f := range m {
			if !known[f] {
				t.Errorf("%s has stale field %s", name, f)
			}
		}
		for _, f := range ProfileFields {
			if _, ok := m[f]; !ok {
				t.Errorf("%s omits %s", name, f)
			}
		}
	}
}
func TestV1Adapter(t *testing.T) {
	for _, name := range []string{"coding-v1", "interactive-v1"} {
		e, _ := ExecByName(name)
		n, tr, err := Resolve(TrustPosture{Selected: TrustedLocal, Posture: FloorFor(TrustedLocal)}, e, Overrides{})
		if err != nil {
			t.Fatal(err)
		}
		if n.RequireDiff || n.ReadWindow != 0 || n.ReadOutline || n.ChurnNudgeRuns != 0 || n.NavSpiralWindow != 4 || n.AnswerNudgeWindow != 0 || n.AutoVerifySoft {
			t.Fatalf("%s generation-one adapter drifted: %+v", name, n)
		}
		if !tr.Canonical {
			t.Fatalf("%s defaults must be canonical", name)
		}
		for _, f := range ProfileFields {
			if tr.Fields[string(f)].Source != SourceProfile {
				t.Errorf("%s %s source=%s", name, f, tr.Fields[string(f)].Source)
			}
		}
	}
}
