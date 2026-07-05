package agent

import (
	"strings"
	"testing"
)

func TestFormatDepDigest(t *testing.T) {
	fixture := `{
		"Path": "github.com/main/mod",
		"Main": true,
		"GoVersion": "1.21"
	}
	{
		"Path": "github.com/direct/mod",
		"Version": "v1.0.0"
	}
	{
		"Path": "github.com/indirect/mod",
		"Version": "v2.0.0",
		"Indirect": true
	}
	{
		"Path": "github.com/replaced/mod",
		"Version": "v0.5.0",
		"Replace": {
			"Path": "./local/mod"
		}
	}
	{
		"Path": "github.com/replaced-ver/mod",
		"Version": "v0.6.0",
		"Replace": {
			"Path": "github.com/fork/mod",
			"Version": "v0.6.1"
		}
	}
	`

	t.Run("basic", func(t *testing.T) {
		got := formatDepDigest(fixture, 10)
		if !strings.Contains(got, "module github.com/main/mod, go 1.21") {
			t.Errorf("missing main module info: %q", got)
		}
		if !strings.Contains(got, "github.com/direct/mod v1.0.0") {
			t.Errorf("missing direct dep: %q", got)
		}
		if strings.Contains(got, "github.com/indirect/mod") {
			t.Errorf("should not contain indirect dep: %q", got)
		}
		if !strings.Contains(got, "github.com/replaced/mod => ./local/mod") {
			t.Errorf("missing replaced dep: %q", got)
		}
		if !strings.Contains(got, "github.com/replaced-ver/mod => github.com/fork/mod@v0.6.1") {
			t.Errorf("missing replaced dep with version: %q", got)
		}
		if !strings.Contains(got, "library versions above are pinned") {
			t.Errorf("missing nudge: %q", got)
		}
	})

	t.Run("overflow", func(t *testing.T) {
		// 3 direct deps in fixture (direct, replaced, replaced-ver)
		got := formatDepDigest(fixture, 2)
		if !strings.Contains(got, "github.com/direct/mod v1.0.0") {
			t.Errorf("missing first direct dep: %q", got)
		}
		if !strings.Contains(got, "… (+1 more direct; go_doc a package for details)") {
			t.Errorf("missing overflow marker: %q", got)
		}
	})

	t.Run("no main", func(t *testing.T) {
		got := formatDepDigest(`{"Path": "foo", "Version": "v1"}`, 10)
		if got != "" {
			t.Errorf("expected empty output for no main module, got %q", got)
		}
	})

	t.Run("multi-main", func(t *testing.T) {
		multiMainFixture := `{
			"Path": "github.com/root/mod",
			"Main": true,
			"GoVersion": "1.26"
		}
		{
			"Path": "github.com/sibling/mod",
			"Main": true
		}
		{
			"Path": "github.com/direct/mod",
			"Version": "v1.0.0"
		}
		`
		got := formatDepDigest(multiMainFixture, 10)
		if !strings.Contains(got, "module github.com/root/mod, go 1.26") {
			t.Errorf("header should contain first main module: %q", got)
		}
		if strings.Contains(got, "github.com/sibling/mod") {
			t.Errorf("header should NOT contain sibling main module: %q", got)
		}
		if !strings.Contains(got, "github.com/direct/mod v1.0.0") {
			t.Errorf("missing direct dep: %q", got)
		}
	})
}
