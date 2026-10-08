package appendrows

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func write(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rows.jsonl")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestALastLineCutShortIsIgnored(t *testing.T) {
	whole := `{"key":"a","done":true}` + "\n" + `{"key":"b","done":true}` + "\n"
	for _, tail := range []string{"", `{"key":"c","do`, `{`, `{"key":"c","done":true}`} {
		got, err := Done(write(t, whole+tail))
		want := map[string]bool{"a": true, "b": true}
		if tail == `{"key":"c","done":true}` {
			want["c"] = true // a whole row that only lacks its line break counts
		}
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("tail %q: got %v, %v; want %v and no error", tail, got, err, want)
		}
	}
}

func TestDamageBeforeTheLastLineIsAnError(t *testing.T) {
	if _, err := Done(write(t, `{"key":"a","done":true}`+"\n{bad\n"+`{"key":"c","done":true}`+"\n")); err == nil {
		t.Error("a damaged line in the middle was accepted")
	}
	if _, err := Done(write(t, `{"key":"a","done":true}`+"\n{bad\n")); err == nil {
		t.Error("a damaged last line that ends with a line break was accepted")
	}
}

func TestTheLastRowForAKeyDecides(t *testing.T) {
	rows := `{"key":"a","done":false}` + "\n" + `{"key":"b","done":true}` + "\n" +
		`{"key":"a","done":true}` + "\n" + `{"key":"c","done":false}` + "\n"
	got, err := Done(write(t, rows))
	want := map[string]bool{"a": true, "b": true, "c": false}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, %v; want %v", got, err, want)
	}
}

func TestAMissingFileIsAnError(t *testing.T) {
	if _, err := Done(filepath.Join(t.TempDir(), "none.jsonl")); err == nil {
		t.Error("a file that does not exist gave no error")
	}
}
