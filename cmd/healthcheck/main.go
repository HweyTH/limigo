// Command healthcheck is a minimal companion binary for the distroless
// limigo image, which has no shell, curl, or wget for Docker's HEALTHCHECK
// to exec. It GETs /healthz and exits 0 on a 200, 1 otherwise.
package main

import (
	"fmt"
	"net/http"
	"os"
	"time"
)

func main() {
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://127.0.0.1:8080/healthz")
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck: /healthz status %d\n", resp.StatusCode)
		os.Exit(1)
	}
}
