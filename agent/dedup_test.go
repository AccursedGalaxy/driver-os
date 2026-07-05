package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/AccursedGalaxy/driver-os/llm"
)

func TestReadOnlyDedup(t *testing.T) {
	// Positive test: two identical consecutive read_file calls with no reasoning advance
	// cause the underlying read to execute ONCE.
	readCount := 0
	tools := map[string]Tool{
		"read_file": {
			Run: func(ctx context.Context, arg string) (string, error) {
				readCount++
				return "file content", nil
			},
		},
		"run": {
			Run: func(ctx context.Context, arg string) (string, error) {
				return "ok", nil
			},
		},
	}

	t.Run("text loop dedup", func(t *testing.T) {
		readCount = 0
		replies := []string{
			"read_file f.txt",
			"read_file f.txt",
			"answer done",
		}
		sp := &scripted{replies: replies}
		_, err := Run(context.Background(), Config{
			Model:   sp,
			Tools:   tools,
			Sandbox: sbWith(t, nil),
		})
		if err != nil {
			t.Fatal(err)
		}
		if readCount != 1 {
			t.Errorf("read_file executed %d times, want 1", readCount)
		}
	})

	t.Run("native loop dedup", func(t *testing.T) {
		readCount = 0
		turns := [][]llm.ContentPart{
			{toolCall("c1", "read_file", "f.txt")},
			{toolCall("c2", "read_file", "f.txt")},
			{llm.Text("done")},
		}
		res, _, _ := runNativeWithTools(t, nil, turns, tools)
		if res.Outcome != Answered {
			t.Fatalf("outcome %v, reason %v", res.Outcome, res.Reason)
		}
		if readCount != 1 {
			t.Errorf("read_file executed %d times, want 1", readCount)
		}
		// Verify the second step has the skip prefix
		if !strings.HasPrefix(res.Steps[1].Observation, "(skipped:") {
			t.Errorf("second step observation missing skip prefix: %q", res.Steps[1].Observation)
		}
	})

	t.Run("negative test: run is never deduped", func(t *testing.T) {
		runCount := 0
		toolsWithRun := map[string]Tool{
			"run": {
				Run: func(ctx context.Context, arg string) (string, error) {
					runCount++
					return "ok", nil
				},
			},
		}
		replies := []string{
			"run echo 1",
			"run echo 1",
			"answer done",
		}
		sp := &scripted{replies: replies}
		Run(context.Background(), Config{
			Model:   sp,
			Tools:   toolsWithRun,
			Sandbox: sbWith(t, nil),
		})
		if runCount != 2 {
			t.Errorf("run executed %d times, want 2", runCount)
		}
	})

	t.Run("text loop negative test: intervening run breaks dedup", func(t *testing.T) {
		readCount = 0
		replies := []string{
			"read_file f.txt",
			"run touch f.txt",
			"read_file f.txt",
			"answer done",
		}
		sp := &scripted{replies: replies}
		_, err := Run(context.Background(), Config{
			Model:   sp,
			Tools:   tools,
			Sandbox: sbWith(t, nil),
		})
		if err != nil {
			t.Fatal(err)
		}
		if readCount != 2 {
			t.Errorf("read_file executed %d times, want 2 (intervening run should break dedup)", readCount)
		}
	})

	t.Run("negative test: intervening run breaks dedup", func(t *testing.T) {
		readCount = 0
		turns := [][]llm.ContentPart{
			{toolCall("c1", "read_file", "f.txt")},
			{toolCall("c2", "run", "touch f.txt")},
			{toolCall("c3", "read_file", "f.txt")},
			{llm.Text("done")},
		}
		runNativeWithTools(t, nil, turns, tools)
		if readCount != 2 {
			t.Errorf("read_file executed %d times, want 2 (intervening run should break dedup)", readCount)
		}
	})
}

func runNativeWithTools(t *testing.T, files map[string]string, turns [][]llm.ContentPart, tools map[string]Tool) (*RunResult, *nativeScript, any) {
	t.Helper()
	sp := &nativeScript{turns: turns}
	sb := sbWith(t, files)
	res, err := RunNative(context.Background(), Config{
		Model:   sp,
		Sandbox: sb,
		Tools:   tools,
	})
	if err != nil {
		t.Fatalf("RunNative: %v", err)
	}
	return res, sp, sb
}
