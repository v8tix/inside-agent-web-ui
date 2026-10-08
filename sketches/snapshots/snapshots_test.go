package snapshots

import (
	"strings"
	"testing"
)

func TestASlowConsumerStillSeesTheWholeText(t *testing.T) {
	var l Latest
	var text strings.Builder
	var seen []string
	for i, delta := range []string{"The", " answer", " is", " 42", "."} {
		text.WriteString(delta)
		l.Publish(text.String()) // every update carries all the text so far
		if i == 1 || i == 4 {    // the consumer is slow: it looks twice only
			got, ok := l.Take()
			if !ok {
				t.Fatalf("nothing to take after update %d", i)
			}
			seen = append(seen, got)
		}
	}
	if want := []string{"The answer", "The answer is 42."}; strings.Join(seen, "|") != strings.Join(want, "|") {
		t.Fatalf("the consumer saw %q, want %q: dropping updates must not damage the text", seen, want)
	}
	if _, ok := l.Take(); ok {
		t.Fatal("a snapshot was handed out twice")
	}
}
