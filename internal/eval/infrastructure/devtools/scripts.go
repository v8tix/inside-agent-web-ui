package devtools

import (
	"embed"
	"encoding/json/v2"
	"regexp"
	"strings"
)

// The functions run inside the page through evaluate_script. They are plain
// JavaScript files in scripts/, each opening with a "// eval:<name>" line that
// names it, so a test server can recognize which one it was given.
//
// A line "// include:<name>" in a script is replaced by the file scripts/<name>.js,
// for the few rules that more than one script must follow the same way (what
// counts as the text of a reply); such a file has no "// eval:" line of its own.
//
//go:embed scripts/*.js
var scriptFS embed.FS

// argPlaceholder stands for the one value a script takes, written into it as a
// JSON string, which is also a valid JavaScript string literal.
const argPlaceholder = "__ARG__"

// includeLine is a line that asks for another file of scripts/.
var includeLine = regexp.MustCompile(`(?m)^[ \t]*// include:(\w+)[ \t]*$`)

func script(name string) string {
	return includeLine.ReplaceAllStringFunc(readScript(name), func(line string) string {
		return strings.TrimRight(readScript(includeLine.FindStringSubmatch(line)[1]), "\n")
	})
}

func readScript(name string) string {
	src, err := scriptFS.ReadFile("scripts/" + name + ".js")
	if err != nil {
		panic("devtools: no script " + name + " (a test keeps this from happening)")
	}
	return string(src)
}

// scriptWith is script with its argument filled in: a string, or a value that
// becomes a JSON object or array.
func scriptWith(name string, arg any) string {
	quoted, err := json.Marshal(arg)
	if err != nil {
		panic(err) // the arguments are strings and small structs of strings
	}
	return strings.ReplaceAll(script(name), argPlaceholder, string(quoted))
}

// scriptMarker returns the name an evaluated function carries on its first line.
func scriptMarker(function string) string {
	first, _, _ := strings.Cut(function, "\n")
	return strings.TrimSpace(strings.TrimPrefix(first, "// eval:"))
}
