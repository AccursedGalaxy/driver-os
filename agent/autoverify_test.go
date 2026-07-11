package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/AccursedGalaxy/driver-os/sandbox"
)

type autoExecSandbox struct {
	sandbox.Sandbox
	exec func(string, time.Duration) *sandbox.Result
}

func (s autoExecSandbox) Exec(ctx context.Context, cmd sandbox.Command) (*sandbox.Result, error) {
	line := strings.Join(cmd.Args, " ")
	if len(cmd.Args) >= 2 && cmd.Args[0] == "-c" {
		line = cmd.Args[1]
	}
	if s.exec != nil {
		return s.exec(line, cmd.Timeout), nil
	}
	return &sandbox.Result{ExitCode: 0}, nil
}

func TestDeriveVerifyCmd(t *testing.T) {
	orig := hasCommand
	defer func() { hasCommand = orig }()
	for _, tc := range []struct {
		name  string
		files map[string]string
		tools map[string]bool
		exec  map[string]int
		want  string
		prov  string
	}{
		{name: "go", files: map[string]string{"go.mod": "module x\n"}, tools: map[string]bool{"go": true}, want: "go build ./... && go test ./...", prov: "go.mod"},
		{name: "go missing", files: map[string]string{"go.mod": "module x\n"}, tools: map[string]bool{}, want: ""},
		{name: "pnpm node", files: map[string]string{"package.json": `{"scripts":{"build":"tsc","test":"vitest"}}`, "pnpm-lock.yaml": ""}, tools: map[string]bool{"pnpm": true}, want: "pnpm run build && pnpm run test", prov: "package.json test script"},
		{name: "bun run form", files: map[string]string{"package.json": `{"scripts":{"test":"jest"}}`, "bun.lockb": ""}, tools: map[string]bool{"bun": true}, want: "bun run test", prov: "package.json test script"},
		{name: "placeholder no tsc", files: map[string]string{"package.json": `{"scripts":{"test":"echo \"Error: no test specified\" && exit 1"}}`}, tools: map[string]bool{"npm": true}, want: ""},
		{name: "pytest", files: map[string]string{"pyproject.toml": "[tool.pytest.ini_options]\n", "tests/x.py": ""}, tools: map[string]bool{"python": true}, exec: map[string]int{"python -c 'import pytest'": 0}, want: "python -m pytest -q", prov: "pytest config"},
		{name: "pytest import fails", files: map[string]string{"pyproject.toml": "[tool.pytest.ini_options]\n", "tests/x.py": ""}, tools: map[string]bool{}, exec: map[string]int{"python -c 'import pytest'": 1}, want: ""},
		{name: "cargo", files: map[string]string{"Cargo.toml": "[package]\n"}, tools: map[string]bool{"cargo": true}, want: "cargo build && cargo test", prov: "Cargo.toml"},
		{name: "make", files: map[string]string{"Makefile": "test:\n\ttrue\n"}, tools: map[string]bool{"make": true}, want: "make test", prov: "Makefile test target"},
		{name: "multi lang", files: map[string]string{"go.mod": "module x\n", "package.json": `{"scripts":{"test":"jest"}}`}, tools: map[string]bool{"go": true, "npm": true}, want: ""},
		{name: "empty", files: nil, tools: map[string]bool{}, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hasCommand = func(ctx context.Context, sb sandbox.Sandbox, tool string) bool { return tc.tools[tool] }
			sb := autoExecSandbox{Sandbox: sbWith(t, tc.files), exec: func(line string, timeout time.Duration) *sandbox.Result {
				if code, ok := tc.exec[line]; ok {
					return &sandbox.Result{ExitCode: code}
				}
				return &sandbox.Result{ExitCode: 0}
			}}
			got, prov := deriveVerifyCmd(context.Background(), sb, ".")
			if got != tc.want || prov != tc.prov {
				t.Fatalf("deriveVerifyCmd = (%q,%q), want (%q,%q)", got, prov, tc.want, tc.prov)
			}
		})
	}
}

func TestResolveAutoVerifyPrecedenceAndIsolation(t *testing.T) {
	orig := hasCommand
	defer func() { hasCommand = orig }()
	hasCommand = func(ctx context.Context, sb sandbox.Sandbox, tool string) bool { return true }
	for _, tc := range []struct {
		name string
		cfg  Config
		want string
	}{
		{name: "explicit wins", cfg: Config{VerifyCmd: "custom", AutoVerify: true, Root: "."}, want: "custom"},
		{name: "off", cfg: Config{AutoVerify: false, Root: "."}, want: ""},
		{name: "untrusted", cfg: Config{AutoVerify: true, Root: ".", MinIsolation: sandbox.IsolationProcess}, want: ""},
		{name: "derives", cfg: Config{AutoVerify: true, Root: "."}, want: "go build ./... && go test ./..."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			cfg.Sandbox = autoExecSandbox{Sandbox: sbWith(t, map[string]string{"go.mod": "module x\n"})}
			cfg.Obs = nopObserver{}
			resolveAutoVerifyOld(context.Background(), &cfg)
			if cfg.VerifyCmd != tc.want {
				t.Fatalf("VerifyCmd = %q, want %q", cfg.VerifyCmd, tc.want)
			}
		})
	}
}

func TestVerifySkipFilter(t *testing.T) {
	for _, tc := range []struct {
		name, cmd, want string
	}{
		{"single quoted", "go test ./agent/ -skip 'TestA|TestB'", "-skip 'TestA|TestB'"},
		{"double quoted", `go test ./agent/ -skip "Test A|Test B"`, `-skip "Test A|Test B"`},
		{"bare", "go test ./agent/ -skip TestA|TestB", "-skip TestA|TestB"},
		{"equals single quoted", "go test ./agent/ -skip='TestA|TestB'", "-skip 'TestA|TestB'"},
		{"equals double quoted", `go test ./agent/ -skip="Test A|Test B"`, `-skip "Test A|Test B"`},
		{"equals bare", "go test ./agent/ -skip=TestA|TestB", "-skip TestA|TestB"},
		{"absent", "go test ./agent/ -run TestA", ""},
		{"first flag wins", "go test ./agent/ -skip TestA -skip TestB", "-skip TestA"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := verifySkipFilter(tc.cmd); got != tc.want {
				t.Fatalf("verifySkipFilter(%q) = %q, want %q", tc.cmd, got, tc.want)
			}
		})
	}
}

func TestVerifyGatePreamble(t *testing.T) {
	cfg := Config{VerifyCmd: "go test ./..."}
	got := verifyGatePreambleT(cfg)
	want := "\n\nVERIFY GATE: `go test ./...`. The harness runs this authoritatively when you finish. If you want mid-run signal, run ONLY tests scoped to the package(s) you changed (for example, `go test ./pkg/you/changed/`). NEVER run the full suite or the full verify command yourself."
	if got != want {
		t.Fatalf("preamble = %q, want %q", got, want)
	}
	cfg.VerifyCmd = "go test ./agent/ -skip 'TestA|TestB'"
	if got := verifyGatePreambleT(cfg); !strings.Contains(got, "`go test ./pkg/you/changed/ -skip 'TestA|TestB'`") || !strings.Contains(got, "The gate deliberately skips these — carry the filter") {
		t.Fatalf("preamble missing skip guidance: %q", got)
	}
	cfg.VerifyCmd = "go test ./..."
	cfg.autoVerifyProvenance = "go.mod"
	if got := verifyGatePreambleT(cfg); !strings.Contains(got, "auto-derived from go.mod") {
		t.Fatalf("preamble missing provenance: %q", got)
	}
}

// satisfy sandbox.Sandbox for compile-time drift checks in this test file.
var _ sandbox.Sandbox = autoExecSandbox{}

func TestResolveAutoVerifyDisarmsRedAndTimeoutBaseline(t *testing.T) {
	orig := hasCommand
	defer func() { hasCommand = orig }()
	hasCommand = func(ctx context.Context, sb sandbox.Sandbox, tool string) bool { return true }
	for _, tc := range []struct {
		name string
		res  *sandbox.Result
	}{
		{name: "red", res: &sandbox.Result{ExitCode: 1}},
		{name: "timeout", res: &sandbox.Result{ExitCode: 124, TimedOut: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{AutoVerify: true, Root: ".", Obs: nopObserver{}, Sandbox: autoExecSandbox{
				Sandbox: sbWith(t, map[string]string{"go.mod": "module x\n"}),
				exec: func(line string, timeout time.Duration) *sandbox.Result {
					if timeout != autoVerifyBaselineTimeout {
						t.Fatalf("baseline timeout = %s, want %s", timeout, autoVerifyBaselineTimeout)
					}
					return tc.res
				},
			}}
			resolveAutoVerifyOld(context.Background(), &cfg)
			if cfg.VerifyCmd != "" || cfg.AutoVerifySoft {
				t.Fatalf("auto verify carried after bad baseline: VerifyCmd=%q soft=%v", cfg.VerifyCmd, cfg.AutoVerifySoft)
			}
			if !cfg.SkipVerifyBaseline {
				t.Fatal("SkipVerifyBaseline = false, want true after auto baseline")
			}
		})
	}
}

func TestAutoVerifySoftAcceptsAfterFeedback(t *testing.T) {
	cfg := Config{
		Sandbox:            autoExecSandbox{Sandbox: sbWith(t, nil), exec: func(string, time.Duration) *sandbox.Result { return &sandbox.Result{ExitCode: 1} }},
		VerifyCmd:          "false",
		SkipVerifyBaseline: true,
		AutoVerifySoft:     true,
		VerifyContinue:     true,
		Obs:                nopObserver{},
	}
	g := mustNewGates(t, context.Background(), cfg, time.Second)
	in := finishInput{answer: "done", canContinue: true, verifyContinuePhrase: "you said done"}
	for i := 0; i < autoVerifyMaxFeedback; i++ {
		if d := g.finish(context.Background(), in); d.kind != finishContinue {
			t.Fatalf("finish %d kind = %v, want continue", i+1, d.kind)
		}
	}
	if d := g.finish(context.Background(), in); d.kind != finishAnswered {
		t.Fatalf("post-budget finish kind = %v outcome=%v reason=%q, want answered", d.kind, d.outcome, d.reason)
	}
}

func TestExplicitVerifyStillUnverified(t *testing.T) {
	cfg := Config{
		Sandbox:            autoExecSandbox{Sandbox: sbWith(t, nil), exec: func(string, time.Duration) *sandbox.Result { return &sandbox.Result{ExitCode: 1} }},
		VerifyCmd:          "false",
		SkipVerifyBaseline: true,
		VerifyContinue:     true,
		Obs:                nopObserver{},
	}
	g := mustNewGates(t, context.Background(), cfg, time.Second)
	d := g.finish(context.Background(), finishInput{answer: "done", canContinue: false})
	if d.kind != finishStop || d.outcome != Unverified {
		t.Fatalf("explicit failing verify = kind %v outcome %v, want stop/unverified", d.kind, d.outcome)
	}
}

func TestVerifyGatePreambleSeededInFirstMessage(t *testing.T) {
	sp := &scripted{replies: []string{"answer done"}}
	res, err := runT(context.Background(), Config{
		Model:              sp,
		Sandbox:            sbWith(t, nil),
		Task:               "t",
		VerifyCmd:          "true",
		SkipVerifyBaseline: true,
	})
	if err != nil || res.Outcome != Answered {
		t.Fatalf("Run outcome=%v err=%v reason=%q", res.Outcome, err, res.Reason)
	}
	if len(res.Messages) == 0 || !strings.Contains(res.Messages[0].Text(), "VERIFY GATE: `true`") || !strings.Contains(res.Messages[0].Text(), "run ONLY tests scoped to the package(s) you changed") || !strings.Contains(res.Messages[0].Text(), "NEVER run the full suite or the full verify command yourself") {
		t.Fatalf("first message missing verify preamble: %#v", res.Messages)
	}
}
