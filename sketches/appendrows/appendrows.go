// Package appendrows reads an append-only file of JSON lines such as {"key":"a","done":true}.
package appendrows

import (
	"bytes"
	"encoding/json"
	"os"
)

// Done reports for each key what its last row says. A last line that a crash cut short is
// ignored; a line that cannot be read anywhere else is damage, and Done returns the error.
func Done(path string) (map[string]bool, error) {
	data, err := os.ReadFile(path)
	done, lines := map[string]bool{}, bytes.Split(data, []byte{'\n'})
	for i, line := range lines {
		var row struct {
			Key  string
			Done bool
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if err := json.Unmarshal(line, &row); err != nil {
			if i == len(lines)-1 {
				break // no line break after it: the write was cut short
			}
			return nil, err
		}
		done[row.Key] = row.Done // a later row for the same key replaces an earlier one
	}
	return done, err
}
