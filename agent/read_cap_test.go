package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestReadFileLineCaps(t *testing.T) {
	file := func(lines int) string {
		var b strings.Builder
		for n := 1; n <= lines; n++ {
			fmt.Fprintf(&b, "line %d\n", n)
		}
		return b.String()
	}
	numberedLines := func(out string) int {
		n := 0
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, "| ") {
				n++
			}
		}
		return n
	}

	for _, tc := range []struct {
		name         string
		fileLines    int
		read         func(t *testing.T, sbBody string) string
		wantLines    int
		wantFooter   string
		forbidFooter bool
	}{
		{
			name:      "closed text range below explicit cap is complete",
			fileLines: 300,
			read: func(t *testing.T, sbBody string) string {
				t.Helper()
				out, err := toolReadFile(context.Background(), sbWith(t, map[string]string{"f.txt": sbBody}), "f.txt:1-260", ReadOptions{})
				if err != nil {
					t.Fatal(err)
				}
				return out
			},
			wantLines:    260,
			forbidFooter: true,
		},
		{
			name:      "closed text range clips at explicit cap and preserves requested end",
			fileLines: 900,
			read: func(t *testing.T, sbBody string) string {
				t.Helper()
				out, err := toolReadFile(context.Background(), sbWith(t, map[string]string{"f.txt": sbBody}), "f.txt:1-800", ReadOptions{})
				if err != nil {
					t.Fatal(err)
				}
				return out
			},
			wantLines:  600,
			wantFooter: "read `f.txt:601-800` for the next chunk",
		},
		{
			name:      "unbounded text read keeps standard cap and fixed width hint",
			fileLines: 200,
			read: func(t *testing.T, sbBody string) string {
				t.Helper()
				out, err := toolReadFile(context.Background(), sbWith(t, map[string]string{"f.txt": sbBody}), "f.txt", ReadOptions{})
				if err != nil {
					t.Fatal(err)
				}
				return out
			},
			wantLines:  150,
			wantFooter: "read `f.txt:151-300` for the next chunk",
		},
		{
			name:      "open-ended text range keeps standard cap",
			fileLines: 400,
			read: func(t *testing.T, sbBody string) string {
				t.Helper()
				out, err := toolReadFile(context.Background(), sbWith(t, map[string]string{"f.txt": sbBody}), "f.txt:40-", ReadOptions{})
				if err != nil {
					t.Fatal(err)
				}
				return out
			},
			wantLines:  150,
			wantFooter: "read `f.txt:190-339` for the next chunk",
		},
		{
			name:      "operator window override bounds closed ranges too",
			fileLines: 300,
			read: func(t *testing.T, sbBody string) string {
				t.Helper()
				out, err := toolReadFile(context.Background(), sbWith(t, map[string]string{"f.txt": sbBody}), "f.txt:1-260", ReadOptions{Window: 40})
				if err != nil {
					t.Fatal(err)
				}
				return out
			},
			wantLines:  40,
			wantFooter: "read `f.txt:41-260` for the next chunk",
		},
		{
			name:      "native closed range receives explicit cap",
			fileLines: 300,
			read: func(t *testing.T, sbBody string) string {
				t.Helper()
				tools := DefaultTools(sbWith(t, map[string]string{"f.txt": sbBody}), time.Second)
				raw, err := json.Marshal(map[string]any{"path": "f.txt", "from": 1, "to": 260})
				if err != nil {
					t.Fatal(err)
				}
				out, err := tools["read_file"].RunJSON(context.Background(), raw)
				if err != nil {
					t.Fatal(err)
				}
				return out
			},
			wantLines:    260,
			forbidFooter: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := tc.read(t, file(tc.fileLines))
			if got := numberedLines(out); got != tc.wantLines {
				t.Errorf("returned %d numbered lines, want %d\n%s", got, tc.wantLines, out)
			}
			if tc.wantFooter != "" && !strings.Contains(out, tc.wantFooter) {
				t.Errorf("footer = %q, want it to contain %q", out, tc.wantFooter)
			}
			if tc.forbidFooter && strings.Contains(out, "more line(s)") {
				t.Errorf("complete read unexpectedly has a truncation footer:\n%s", out)
			}
		})
	}
}
