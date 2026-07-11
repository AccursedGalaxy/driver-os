package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestConfigReadWiring(t *testing.T) {
	src := `package main
type Server struct{}
func (s *Server) Serve() {}
`
	for i := 0; i < 100; i++ {
		src += fmt.Sprintf("// line %d\n", i+4)
	}
	files := map[string]string{"main.go": src}

	t.Run("custom window and outline", func(t *testing.T) {
		cfg := Config{
			Sandbox:     sbWith(t, files),
			ReadWindow:  20,
			ReadOutline: true,
		}
		tools := wrapToolsT(cfg, 30*time.Second)
		read, ok := tools["read_file"]
		if !ok {
			t.Fatal("read_file tool not found")
		}

		obs, err := read.Run(context.Background(), "main.go")
		if err != nil {
			t.Fatalf("read_file failed: %v", err)
		}

		lines := strings.Split(strings.TrimSpace(obs), "\n")
		// We expect 20 lines of content + 1 footer line + outline header + 2 symbols
		// But let's just check the properties.

		// 1. Check window: the last numbered line should be 20
		found20 := false
		for _, l := range lines {
			if strings.HasPrefix(l, "20| ") {
				found20 = true
				break
			}
		}
		if !found20 {
			t.Errorf("expected line 20 in output, got:\n%s", obs)
		}

		// Check that line 21 is NOT there as a numbered line
		for _, l := range lines {
			if strings.HasPrefix(l, "21| ") {
				t.Errorf("did not expect line 21 in output")
				break
			}
		}

		// 2. Check outline
		if !strings.Contains(obs, "--- file outline (2 symbols)") {
			t.Error("expected file outline header")
		}
		if !strings.Contains(obs, "func (*Server) Serve") {
			t.Error("expected symbol in outline")
		}
	})

	t.Run("zero config defaults", func(t *testing.T) {
		cfg := Config{
			Sandbox: sbWith(t, files),
		}
		tools := wrapToolsT(cfg, 30*time.Second)
		read := tools["read_file"]

		obs, err := read.Run(context.Background(), "main.go")
		if err != nil {
			t.Fatal(err)
		}

		// Default window is 150, our file is ~103 lines, so it should NOT clip
		// and should NOT have an outline.
		if strings.Contains(obs, "file outline") {
			t.Error("expected no file outline for small file with default config")
		}

		// Verify it didn't clip (line 100 should be there)
		if !strings.Contains(obs, "100| // line 100") {
			t.Error("expected line 100 in unclipped output")
		}
	})
}
