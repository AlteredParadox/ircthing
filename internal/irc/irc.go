// Package irc manages per-network IRC connections: one read-loop goroutine
// and one rate-limited writer goroutine per connection, with reconnection
// (exponential backoff + jitter). Consumers (the hub) receive server
// messages and connection-state changes from the Manager's Events channel
// and queue outbound messages with Send.
package irc
