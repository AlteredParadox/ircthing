// Package hub owns fan-out from IRC events to connected WebSocket
// sessions. For now it implements the persistence half: consuming a
// connection manager's events and appending message traffic to the store.
// WebSocket sessions attach here in a later phase.
package hub

import (
	"context"
	"log"
	"strings"
	"time"

	"ircthing/internal/irc"
	"ircthing/internal/store"

	ircv4 "gopkg.in/irc.v4"
)

type Hub struct {
	store *store.Store
}

func New(st *store.Store) *Hub {
	return &Hub{store: st}
}

// Conn is the slice of *irc.Manager the hub consumes.
type Conn interface {
	Events() <-chan irc.Event
	Nick() string
}

// Run consumes one network's events and persists message traffic until
// ctx is canceled. A failed append is logged, not fatal: losing one line
// of scrollback beats dropping the connection's event stream.
func (h *Hub) Run(ctx context.Context, c Conn) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev := <-c.Events():
			switch ev.Kind {
			case irc.EventState:
				if ev.Err != nil {
					log.Printf("irc[%s]: %s: %v", ev.Network, ev.State, ev.Err)
				} else {
					log.Printf("irc[%s]: %s", ev.Network, ev.State)
				}
			case irc.EventMessage:
				target, ok := persistTarget(ev.Msg, c.Nick())
				if !ok {
					continue
				}
				if _, err := h.store.Append(ctx, ev.Network, target, storeMessage(ev)); err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					log.Printf("irc[%s]: persist %s to %q: %v", ev.Network, ev.Msg.Command, target, err)
				}
			}
		}
	}
}

// persistTarget decides which buffer a message lands in, or none.
// Channel detection uses the default CHANTYPES "#&" until ISUPPORT
// parsing lands.
func persistTarget(m *ircv4.Message, ourNick string) (string, bool) {
	switch m.Command {
	case "PRIVMSG", "NOTICE":
		t := m.Param(0)
		if isChannel(t) {
			return t, true
		}
		// Addressed to us: file under the sender (queries, NickServ,
		// server notices under the server's name).
		if m.Prefix != nil && m.Prefix.Name != "" &&
			ourNick != "" && strings.EqualFold(t, ourNick) {
			return m.Prefix.Name, true
		}
		return "", false
	case "JOIN", "PART", "TOPIC", "KICK", "MODE":
		if t := m.Param(0); isChannel(t) {
			return t, true
		}
		return "", false
	}
	return "", false
}

func isChannel(target string) bool {
	return target != "" && (target[0] == '#' || target[0] == '&')
}

// messageTime is the single place the stored-timestamp source rule
// lives. Precedence:
//
//  1. the message's IRCv3 server-time tag, when present and valid
//     (RFC3339 UTC with millisecond precision) — the server's clock is
//     authoritative for ordering across reconnects and future history
//     replay;
//  2. otherwise the local receive time carried on the event;
//  3. as a last resort (zero event time), store.Append stamps the
//     current time on insert.
func messageTime(ev irc.Event) time.Time {
	if v, ok := ev.Msg.Tags["time"]; ok {
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			return t
		}
	}
	return ev.Time
}

// storeMessage converts an event to its stored form.
func storeMessage(ev irc.Event) store.Message {
	sender := ""
	if ev.Msg.Prefix != nil {
		sender = ev.Msg.Prefix.Name
	}
	return store.Message{
		Time:    messageTime(ev),
		MsgID:   ev.Msg.Tags["msgid"],
		Sender:  sender,
		Command: ev.Msg.Command,
		Raw:     ev.Msg.String(),
	}
}
