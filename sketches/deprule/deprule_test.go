package deprule

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// write puts one Go file with one import into dir.
func write(t *testing.T, dir, name, imp string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := "package p\n\nimport _ \"" + imp + "\"\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTheRuleNamesTheFileTheImportAndTheReason(t *testing.T) {
	core := filepath.Join(t.TempDir(), "core")
	rule := Rule{What: "the core knows nothing of the web", Dir: core, Banned: "example.test/shop/web"}

	write(t, core, "order.go", "example.test/shop/core/money")
	if got := rule.Check(); len(got) != 0 {
		t.Fatalf("an allowed import was reported: %v", got)
	}

	write(t, core, "invoice.go", "example.test/shop/web/render")
	got := rule.Check()
	if len(got) != 1 {
		t.Fatalf("want one problem, got %v", got)
	}
	for _, part := range []string{"invoice.go", "example.test/shop/web/render", rule.What} {
		if !strings.Contains(got[0], part) {
			t.Errorf("the message %q does not name %q", got[0], part)
		}
	}
}

func TestARuleThatCoversNothingFails(t *testing.T) {
	gone := Rule{What: "the billing code knows nothing of the web", Dir: filepath.Join(t.TempDir(), "billing"), Banned: "example.test/shop/web"}

	got := gone.Check()

	if len(got) != 1 || !strings.Contains(got[0], "no file falls under") {
		t.Fatalf("a rule over a folder that is gone must fail, got %v", got)
	}
}
