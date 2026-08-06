// Package unregistered has non-deterministic code and no workflow. Nothing
// here is diagnosed: reachability from dbos.RegisterWorkflow is what makes a
// clock read a finding, and ordinary server code reads the clock all day.
package unregistered

import (
	"net/http"
	"os"
	"time"
)

func Now() time.Time { return time.Now() } // want Now:"nondeterministic: time.Now"

func Fetch(url string) (*http.Response, error) { return http.Get(url) } // want Fetch:"nondeterministic: net/http"

func DatabaseURL() string { return os.Getenv("AGENTIQ_DATABASE_URL") } // want DatabaseURL:"nondeterministic: os.Getenv"

func Order(m map[string]int) []string { // want Order:"nondeterministic: map-range"
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
