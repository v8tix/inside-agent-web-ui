package digestsessions

import (
	"testing"
	"time"
)

func TestALeakedTableIsNotASession(t *testing.T) {
	table := Table{}
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	token := table.Issue(now.Add(time.Hour))

	if !table.Valid(token, now) {
		t.Fatal("the token that was issued is not accepted")
	}
	for key := range table {
		if key == token {
			t.Fatal("the table holds the token itself")
		}
		if table.Valid(key, now) {
			t.Fatal("a key read from the table works as a token: a leak would hand out sessions")
		}
	}
}

func TestASessionEndsAtItsEndTime(t *testing.T) {
	table := Table{}
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	token := table.Issue(now.Add(time.Hour))

	if table.Valid(token, now.Add(time.Hour)) {
		t.Fatal("the session is still valid at its end time: the end is exclusive")
	}
	if table.Valid(token+"x", now) {
		t.Fatal("a token that was never issued is accepted")
	}
}
