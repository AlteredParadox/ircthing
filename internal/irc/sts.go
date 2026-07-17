package irc

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// STS — strict transport security
// (https://ircv3.net/specs/extensions/sts, fetched 2026-07-15).
//
// The sts capability is never requested with CAP REQ. Its value is a
// comma-separated key=value list; unknown keys are ignored:
//
//   - On an INSECURE connection only the port key matters: the client
//     must close the connection and reconnect securely on that port
//     (with proper certificate verification). A policy without a valid
//     port is treated as not advertised. This upgrade is session-scoped,
//     never persisted.
//   - On a SECURE connection only the duration key matters: the policy
//     (current secure port, expiring duration seconds from now) is
//     persisted so future connects to this host use TLS even if the
//     config says plaintext. duration=0 clears the policy. The expiry is
//     reset every time a duration arrives, and rescheduled to
//     close-time + duration when a connection closes.

// STSStore persists STS policies across restarts, keyed by hostname.
// Implementations must be safe for concurrent use. *store.Store
// implements this on its settings table.
type STSStore interface {
	// STSPolicy returns the stored policy for host; ok is false when none
	// is stored (expiry is not checked here — callers do that).
	STSPolicy(ctx context.Context, host string) (port int, until time.Time, ok bool, err error)
	SetSTSPolicy(ctx context.Context, host string, port int, until time.Time) error
	ClearSTSPolicy(ctx context.Context, host string) error
}

// stsValue is one parsed sts capability value.
type stsValue struct {
	port        int // advertised secure port, 0 when absent/invalid
	hasDuration bool
	duration    time.Duration
}

func parseSTS(value string) stsValue {
	var v stsValue
	for _, tok := range strings.Split(value, ",") {
		k, val, _ := strings.Cut(tok, "=")
		switch k {
		case "port":
			if n, err := strconv.Atoi(val); err == nil && n > 0 && n <= 65535 {
				v.port = n
			}
		case "duration":
			if n, err := strconv.Atoi(val); err == nil && n >= 0 {
				v.hasDuration = true
				// Clamp before the multiply: time.Duration is int64 ns, so
				// n*1e9 overflows past ~9.2e9 seconds and wraps to a garbage
				// (often negative) duration — which would persist a policy
				// that expires in the PAST, silently downgrading future
				// connects to plaintext. Cap at ~100 years (effectively
				// forever for STS) so a huge advertised value stays in force.
				const maxSTSSeconds = 100 * 365 * 24 * 60 * 60
				if n > maxSTSSeconds {
					n = maxSTSSeconds
				}
				v.duration = time.Duration(n) * time.Second
			}
		}
		// Unknown keys (preload, future extensions) are ignored per spec.
	}
	return v
}

// errSTSUpgrade aborts an insecure registration so the manager can redial
// securely on the advertised port. It is a policy-driven upgrade, not a
// failure: the reconnect happens immediately, without backoff.
type errSTSUpgrade struct{ port int }

func (e errSTSUpgrade) Error() string {
	return fmt.Sprintf("sts: upgrading to TLS on port %d", e.port)
}
