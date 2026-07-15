// Package hub owns fan-out from IRC events to connected WebSocket sessions.
// Components communicate over channels; shared mutable state is confined
// here and in the store.
package hub
