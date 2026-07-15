// Package hub owns fan-out: IRC events flow in from the per-network
// connection managers, get persisted to the store, and are broadcast to
// every connected WebSocket session. Sessions send client requests
// (history pages, read markers, outgoing messages) back through it.
// The hub is transport-agnostic: internal/api owns the actual WebSocket
// connections and moves Envelopes in and out of Sessions.
package hub

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"ircthing/internal/irc"
	"ircthing/internal/store"

	ircv4 "gopkg.in/irc.v4"
)

// Conn is the slice of *irc.Manager the hub consumes.
type Conn interface {
	Name() string
	Nick() string
	Events() <-chan irc.Event
	Send(*ircv4.Message) error
	Channel(name string) (topic string, members []irc.Member, ok bool)
	CapEnabled(name string) bool
	IsChannel(target string) bool // per the network's ISUPPORT CHANTYPES
	ChanTypes() string
}

type Hub struct {
	store *store.Store

	mu       sync.Mutex
	networks map[string]Conn
	states   map[string]string // last known connection state per network
	sessions map[*Session]struct{}
}

func New(st *store.Store) *Hub {
	return &Hub{
		store:    st,
		networks: make(map[string]Conn),
		states:   make(map[string]string),
		sessions: make(map[*Session]struct{}),
	}
}

// Run consumes one network's events until ctx is canceled: message
// traffic is persisted and broadcast, state changes are broadcast. A
// failed append is logged, not fatal: losing one line of scrollback beats
// dropping the connection's event stream.
func (h *Hub) Run(ctx context.Context, c Conn) error {
	name := c.Name()
	h.mu.Lock()
	h.networks[name] = c
	if _, ok := h.states[name]; !ok {
		h.states[name] = irc.StateDisconnected.String()
	}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.networks, name)
		h.mu.Unlock()
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev := <-c.Events():
			switch ev.Kind {
			case irc.EventState:
				h.mu.Lock()
				h.states[ev.Network] = ev.State.String()
				h.mu.Unlock()
				errStr := ""
				if ev.Err != nil {
					errStr = ev.Err.Error()
					log.Printf("irc[%s]: %s: %v", ev.Network, ev.State, ev.Err)
				} else {
					log.Printf("irc[%s]: %s", ev.Network, ev.State)
				}
				h.broadcast(envelope("state", 0, StateData{
					Network: ev.Network,
					State:   ev.State.String(),
					Error:   errStr,
				}))
			case irc.EventMessage:
				if hint, affected := membersHint(ev.Msg); affected {
					h.broadcast(envelope("members_changed", 0, MembersChangedData{
						Network: ev.Network, Buffer: hint,
					}))
				}
				if ev.Msg.Command == "TAGMSG" {
					h.relayTyping(ev, c)
					continue // TAGMSG is ephemeral, never persisted
				}
				target, ok := persistTarget(ev.Msg, c.Nick(), c.IsChannel)
				if !ok {
					continue
				}
				stored, err := h.store.Append(ctx, ev.Network, target, storeMessage(ev))
				if err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					log.Printf("irc[%s]: persist %s to %q: %v", ev.Network, ev.Msg.Command, target, err)
					continue
				}
				h.broadcast(envelope("event", 0, eventData(stored)))
			}
		}
	}
}

func (h *Hub) network(name string) Conn {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.networks[name]
}

func (h *Hub) broadcast(env Envelope) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.sessions {
		s.push(env)
	}
}

// broadcastExcept sends to every session but the originator, which gets
// its own seq-tagged response instead.
func (h *Hub) broadcastExcept(except *Session, env Envelope) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.sessions {
		if s != except {
			s.push(env)
		}
	}
}

// relayTyping turns an incoming TAGMSG carrying the +typing client tag
// into a "typing" push. Our own echoed TAGMSGs are ignored — the local
// client knows what it is typing.
func (h *Hub) relayTyping(ev irc.Event, c Conn) {
	state := ev.Msg.Tags["+typing"]
	if state != "active" && state != "paused" && state != "done" {
		return
	}
	sender := ""
	if ev.Msg.Prefix != nil {
		sender = ev.Msg.Prefix.Name
	}
	nick := c.Nick()
	if sender == "" || nick == "" || strings.EqualFold(sender, nick) {
		return
	}
	buffer := ev.Msg.Param(0)
	if !c.IsChannel(buffer) {
		// A typing notice addressed to us belongs in the sender's query.
		if !strings.EqualFold(buffer, nick) {
			return
		}
		buffer = sender
	}
	h.broadcast(envelope("typing", 0, TypingData{
		Network: ev.Network, Buffer: buffer, Nick: sender, State: state,
	}))
}

// membersHint reports whether a message changes channel state clients may
// be displaying, and which buffer it affects ("" = anywhere on the
// network: QUIT and NICK span channels the hub doesn't track).
func membersHint(m *ircv4.Message) (buffer string, affected bool) {
	switch m.Command {
	case "JOIN", "PART", "KICK", "TOPIC", "MODE":
		return m.Param(0), true
	case "366": // end of NAMES: <me> <channel>
		return m.Param(1), true
	case "QUIT", "NICK", "AWAY": // span channels the hub doesn't track
		return "", true
	}
	return "", false
}

func newPrivmsg(target, text string) *ircv4.Message {
	return &ircv4.Message{Command: "PRIVMSG", Params: []string{target, text}}
}

func eventData(m store.Message) EventData {
	return EventData{
		Network: m.Network,
		Buffer:  m.Target,
		ID:      m.ID,
		Time:    m.Time.UnixMilli(),
		MsgID:   m.MsgID,
		Sender:  m.Sender,
		Command: m.Command,
		Raw:     m.Raw,
	}
}

// persistTarget decides which buffer a message lands in, or none.
// isChan is the network's ISUPPORT-driven channel detection.
func persistTarget(m *ircv4.Message, ourNick string, isChan func(string) bool) (string, bool) {
	switch m.Command {
	case "PRIVMSG", "NOTICE":
		t := m.Param(0)
		if isChan(t) {
			return t, true
		}
		if m.Prefix == nil || m.Prefix.Name == "" || ourNick == "" {
			return "", false
		}
		// Addressed to us: file under the sender (queries, NickServ,
		// server notices under the server's name).
		if strings.EqualFold(t, ourNick) {
			return m.Prefix.Name, true
		}
		// Our own message echoed back (echo-message): file under the
		// recipient so sent PMs land in the query buffer.
		if strings.EqualFold(m.Prefix.Name, ourNick) && t != "" {
			return t, true
		}
		return "", false
	case "JOIN", "PART", "TOPIC", "KICK", "MODE":
		if t := m.Param(0); isChan(t) {
			return t, true
		}
		return "", false
	}
	return "", false
}

// defaultIsChannel is the RFC 1459 CHANTYPES fallback, used by tests.
func defaultIsChannel(target string) bool {
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
