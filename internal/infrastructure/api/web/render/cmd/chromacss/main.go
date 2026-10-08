// Command chromacss writes the syntax-highlighting stylesheet.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/v8tix/inside-agent-web-ui/internal/infrastructure/api/web/render"
)

func main() {
	out := flag.String("o", "", "file to write (default: standard output)")
	flag.Parse()

	css := render.ChromaCSS()
	if *out == "" {
		fmt.Print(css)
		return
	}
	if err := os.WriteFile(*out, []byte(css), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "chromacss:", err)
		os.Exit(1)
	}
}
