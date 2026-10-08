package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// healthcheck asks a running server whether it is ready: it exits 0 when
// /readyz answers 200. It exists for container health checks, because the
// image has no shell or curl. With no argument it asks the address the server
// itself would listen on (APP_HTTP_ADDR).
func healthcheck(args []string, getenv func(string) string) int {
	url := "http://127.0.0.1:" + portOf(getenv("APP_HTTP_ADDR")) + "/readyz"
	if len(args) > 0 {
		url = args[0]
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintln(os.Stderr, "unhealthy:", err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
		fmt.Fprintf(os.Stderr, "unhealthy: status %d, body %q\n", resp.StatusCode, body)
		return 1
	}
	return 0
}

// portOf is the port of a listen address such as ":8300" or "0.0.0.0:8300".
func portOf(addr string) string {
	if _, port, ok := strings.Cut(addr, ":"); ok && port != "" {
		return port[strings.LastIndex(port, ":")+1:]
	}
	return "8300"
}
