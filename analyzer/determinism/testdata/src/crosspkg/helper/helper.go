// Package helper is a second package of the same module. The analyzer follows
// calls into it — and, in the same fixture, does not follow calls into `time`
// or `net/http`, because walking the standard library would report that
// `encoding/json` ranges over maps and drown every real finding.
package helper

import "time"

// Stamp is non-deterministic and carries a fact saying so.
func Stamp() int64 { // want Stamp:"nondeterministic: time.Now"
	return time.Now().UnixNano()
}

// Label is deterministic and carries no fact.
func Label(digest string) string {
	if digest == "" {
		return "PENDING"
	}
	return "SUCCESS"
}
