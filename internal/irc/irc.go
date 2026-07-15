// Package irc manages per-network IRC connections: one read-loop goroutine
// and one rate-limited writer goroutine per network, with reconnection
// (exponential backoff + jitter). Events flow out through internal/hub.
package irc
