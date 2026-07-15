package hub

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"ircthing/internal/store"

	ircv4 "gopkg.in/irc.v4"
)

// sessionBuffer is the per-session outbound queue depth. A session that
// falls this far behind is disconnected (see push).
var sessionBuffer = 64

// Session is one connected client (one browser tab / device). The
// transport (internal/api) feeds client envelopes to Handle and writes
// everything from Outbound to the wire; when Done closes, the transport
// must drop the connection.
type Session struct {
	hub  *Hub
	out  chan Envelope
	done chan struct{}
	once sync.Once
}

func (h *Hub) NewSession() *Session {
	s := &Session{
		hub:  h,
		out:  make(chan Envelope, sessionBuffer),
		done: make(chan struct{}),
	}
	h.mu.Lock()
	h.sessions[s] = struct{}{}
	h.mu.Unlock()
	return s
}

// Close unregisters the session; the transport calls it exactly once when
// the connection ends.
func (s *Session) Close() {
	s.hub.mu.Lock()
	delete(s.hub.sessions, s)
	s.hub.mu.Unlock()
	s.disconnect()
}

func (s *Session) Outbound() <-chan Envelope { return s.out }

// Done closes when the session has been evicted (or Closed); the
// transport must stop writing and drop the connection.
func (s *Session) Done() <-chan struct{} { return s.done }

// push enqueues without blocking. A session too slow to drain its buffer
// is disconnected rather than stalling the hub or silently dropping
// events mid-stream — after a reconnect the client refetches history and
// misses nothing.
func (s *Session) push(env Envelope) {
	select {
	case <-s.done:
	case s.out <- env:
	default:
		s.disconnect()
	}
}

func (s *Session) disconnect() {
	s.once.Do(func() { close(s.done) })
}

// Handle processes one client envelope. Responses and errors are pushed
// to this session's Outbound, tagged with the request's Seq; side effects
// other sessions must see are broadcast.
func (s *Session) Handle(ctx context.Context, env Envelope) {
	if env.V != ProtocolVersion {
		return // unknown protocol version: ignore
	}
	switch env.Type {
	case "send":
		s.handleSend(ctx, env)
	case "command":
		s.handleCommand(ctx, env)
	case "redact":
		s.handleRedact(ctx, env)
	case "get_channel":
		s.handleGetChannel(ctx, env)
	case "get_buffers":
		s.handleGetBuffers(ctx, env)
	case "typing":
		s.handleTyping(ctx, env)
	case "get_history":
		s.handleGetHistory(ctx, env)
	case "search":
		s.handleSearch(ctx, env)
	case "get_read_marker":
		s.handleGetMarker(ctx, env)
	case "set_read_marker":
		s.handleSetMarker(ctx, env)
	default:
		// Unknown message types are ignored, not errored (protocol
		// forward-compatibility rule).
	}
}

func (s *Session) handleSend(ctx context.Context, env Envelope) {
	var d SendData
	if err := json.Unmarshal(env.Data, &d); err != nil {
		s.push(errEnvelope(env.Seq, "bad_request", "malformed send data"))
		return
	}
	if d.Network == "" || d.Target == "" || strings.TrimSpace(d.Text) == "" {
		s.push(errEnvelope(env.Seq, "bad_request", "send needs network, target and text"))
		return
	}
	conn := s.hub.network(d.Network)
	if conn == nil {
		s.push(errEnvelope(env.Seq, "unknown_network", "network is not connected"))
		return
	}
	// With echo-message negotiated the server reflects our PRIVMSGs and
	// the regular event path persists them (with server-time and msgid);
	// otherwise persist and broadcast ourselves so every device sees
	// what this one sent.
	echo := conn.CapEnabled("echo-message")
	for _, line := range strings.Split(strings.ReplaceAll(d.Text, "\r\n", "\n"), "\n") {
		if line == "" {
			continue
		}
		msg := newPrivmsg(d.Target, line)
		if err := conn.Send(msg); err != nil {
			s.push(errEnvelope(env.Seq, "send_failed", err.Error()))
			return
		}
		if echo {
			continue
		}
		stored, err := s.hub.store.Append(ctx, d.Network, d.Target, store.Message{
			Time:    time.Now(),
			Sender:  conn.Nick(),
			Command: "PRIVMSG",
			Raw:     msg.String(),
			Text:    line,
		})
		if err != nil {
			s.push(errEnvelope(env.Seq, "persist_failed", err.Error()))
			return
		}
		s.hub.broadcast(envelope("event", 0, eventData(stored)))
	}
	s.push(envelope("ok", env.Seq, nil))
}

// handleTyping relays our typing state as a TAGMSG with the +typing
// client tag (https://ircv3.net/specs/client-tags/typing, fetched
// 2026-07-15). Best-effort: on a network without message-tags (the
// transport requirement) it is a silent no-op. Acked only when the client
// asked (Seq set) — typing is fire-and-forget.
func (s *Session) handleTyping(ctx context.Context, env Envelope) {
	var d TypingData
	if err := json.Unmarshal(env.Data, &d); err != nil {
		return
	}
	ok := d.Network != "" && d.Buffer != "" &&
		(d.State == "active" || d.State == "paused" || d.State == "done")
	if ok {
		if conn := s.hub.network(d.Network); conn != nil && conn.CapEnabled("message-tags") {
			_ = conn.Send(&ircv4.Message{
				Tags:    ircv4.Tags{"+typing": d.State},
				Command: "TAGMSG",
				Params:  []string{d.Buffer},
			})
		}
	}
	if env.Seq != 0 {
		s.push(envelope("ok", env.Seq, nil))
	}
}

// handleRedact issues a REDACT for one of our messages
// (draft/message-redaction). The server decides authorization; on success
// it echoes the REDACT back, which applyRedaction then stores and
// broadcasts. A no-op on networks without the cap.
func (s *Session) handleRedact(ctx context.Context, env Envelope) {
	var d RedactReq
	if err := json.Unmarshal(env.Data, &d); err != nil {
		s.push(errEnvelope(env.Seq, "bad_request", "malformed redact data"))
		return
	}
	if d.Network == "" || d.Buffer == "" || d.MsgID == "" {
		s.push(errEnvelope(env.Seq, "bad_request", "redact needs network, buffer and msgid"))
		return
	}
	conn := s.hub.network(d.Network)
	if conn == nil {
		s.push(errEnvelope(env.Seq, "unknown_network", "network is not connected"))
		return
	}
	if !conn.CapEnabled("draft/message-redaction") {
		s.push(errEnvelope(env.Seq, "unsupported", "server does not support message redaction"))
		return
	}
	params := []string{d.Buffer, d.MsgID}
	if d.Reason != "" {
		params = append(params, d.Reason)
	}
	if err := conn.Send(&ircv4.Message{Command: "REDACT", Params: params}); err != nil {
		s.push(errEnvelope(env.Seq, "send_failed", err.Error()))
		return
	}
	s.push(envelope("ok", env.Seq, nil))
}

// commandSpec is the allowlist of client-issued IRC commands with their
// permitted parameter counts. Everything else is rejected — the client
// does not get raw connection access.
var commandSpec = map[string]struct{ min, max int }{
	"JOIN":  {1, 2}, // channel [key]
	"PART":  {1, 2}, // channel [reason]
	"NICK":  {1, 1},
	"TOPIC": {1, 2}, // channel [new topic]
}

func (s *Session) handleCommand(ctx context.Context, env Envelope) {
	var d CommandData
	if err := json.Unmarshal(env.Data, &d); err != nil {
		s.push(errEnvelope(env.Seq, "bad_request", "malformed command data"))
		return
	}
	cmd := strings.ToUpper(d.Command)
	spec, allowed := commandSpec[cmd]
	if !allowed {
		s.push(errEnvelope(env.Seq, "bad_request", "command not allowed: "+d.Command))
		return
	}
	if len(d.Params) < spec.min || len(d.Params) > spec.max {
		s.push(errEnvelope(env.Seq, "bad_request", "wrong number of parameters"))
		return
	}
	for i, p := range d.Params {
		if p == "" || strings.ContainsAny(p, "\r\n\x00") {
			s.push(errEnvelope(env.Seq, "bad_request", "invalid parameter"))
			return
		}
		// Only the final parameter may contain spaces or lead with ':'
		// (it is sent as the trailing parameter).
		if i < len(d.Params)-1 && (strings.Contains(p, " ") || strings.HasPrefix(p, ":")) {
			s.push(errEnvelope(env.Seq, "bad_request", "invalid parameter"))
			return
		}
	}
	conn := s.hub.network(d.Network)
	if conn == nil {
		s.push(errEnvelope(env.Seq, "unknown_network", "network is not connected"))
		return
	}
	if err := conn.Send(&ircv4.Message{Command: cmd, Params: d.Params}); err != nil {
		s.push(errEnvelope(env.Seq, "send_failed", err.Error()))
		return
	}
	s.push(envelope("ok", env.Seq, nil))
}

func (s *Session) handleGetChannel(ctx context.Context, env Envelope) {
	var d ChannelReq
	if err := json.Unmarshal(env.Data, &d); err != nil {
		s.push(errEnvelope(env.Seq, "bad_request", "malformed get_channel data"))
		return
	}
	data := ChannelData{Network: d.Network, Buffer: d.Buffer, Members: []MemberData{}}
	if conn := s.hub.network(d.Network); conn != nil {
		// Under no-implicit-names the roster is empty until we ask; this
		// fetches lazily the first time a channel is viewed. The 366 reply
		// raises members_changed and the client refetches.
		conn.EnsureNames(d.Buffer)
		topic, members, ok := conn.Channel(d.Buffer)
		if ok {
			data.Joined = true
			data.Topic = topic
			for _, m := range members {
				data.Members = append(data.Members, MemberData{Nick: m.Nick, Prefix: m.Prefix, Away: m.Away})
			}
		}
	}
	s.push(envelope("channel", env.Seq, data))
}

func (s *Session) handleGetBuffers(ctx context.Context, env Envelope) {
	infos, err := s.hub.store.Buffers(ctx)
	if err != nil {
		s.push(errEnvelope(env.Seq, "internal", "buffer list query failed"))
		return
	}
	data := BuffersData{
		Networks: make([]NetworkInfo, 0, 4),
		Buffers:  make([]BufferInfo, 0, len(infos)),
	}
	for _, b := range infos {
		data.Buffers = append(data.Buffers, BufferInfo{
			Network: b.Network, Buffer: b.Target,
			LastTime: b.LastTS, Marker: b.Marker, Unread: b.Unread,
		})
	}
	s.hub.mu.Lock()
	names := make([]string, 0, len(s.hub.states))
	for name := range s.hub.states {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		info := NetworkInfo{Name: name, State: s.hub.states[name]}
		if c := s.hub.networks[name]; c != nil {
			info.Nick = c.Nick()
			info.ChanTypes = c.ChanTypes()
		}
		data.Networks = append(data.Networks, info)
	}
	s.hub.mu.Unlock()
	s.push(envelope("buffers", env.Seq, data))
}

func (s *Session) handleGetHistory(ctx context.Context, env Envelope) {
	var d HistoryReq
	if err := json.Unmarshal(env.Data, &d); err != nil {
		s.push(errEnvelope(env.Seq, "bad_request", "malformed get_history data"))
		return
	}
	if d.Network == "" || d.Buffer == "" {
		s.push(errEnvelope(env.Seq, "bad_request", "get_history needs network and buffer"))
		return
	}

	var (
		msgs []store.Message
		err  error
	)
	switch {
	case d.Before != nil:
		msgs, err = s.hub.store.Before(ctx, d.Network, d.Buffer, store.Cursor(*d.Before), d.Limit)
	case d.After != nil:
		msgs, err = s.hub.store.After(ctx, d.Network, d.Buffer, store.Cursor(*d.After), d.Limit)
	case d.Around != nil:
		msgs, err = s.hub.store.Around(ctx, d.Network, d.Buffer, store.Cursor(*d.Around), d.Limit)
	case d.BeforeMsgID != "":
		var c store.Cursor
		if c, err = s.hub.store.CursorForMsgID(ctx, d.Network, d.Buffer, d.BeforeMsgID); err == nil {
			msgs, err = s.hub.store.Before(ctx, d.Network, d.Buffer, c, d.Limit)
		}
	case d.AfterMsgID != "":
		var c store.Cursor
		if c, err = s.hub.store.CursorForMsgID(ctx, d.Network, d.Buffer, d.AfterMsgID); err == nil {
			msgs, err = s.hub.store.After(ctx, d.Network, d.Buffer, c, d.Limit)
		}
	default:
		msgs, err = s.hub.store.Latest(ctx, d.Network, d.Buffer, d.Limit)
	}
	if errors.Is(err, store.ErrMsgIDNotFound) {
		s.push(errEnvelope(env.Seq, "unknown_msgid", "no message with that msgid"))
		return
	}
	if err != nil {
		s.push(errEnvelope(env.Seq, "internal", "history query failed"))
		return
	}

	page := HistoryData{
		Network:  d.Network,
		Buffer:   d.Buffer,
		Messages: make([]EventData, 0, len(msgs)),
	}
	for _, m := range msgs {
		page.Messages = append(page.Messages, eventData(m))
	}
	s.push(envelope("history", env.Seq, page))
}

func (s *Session) handleSearch(ctx context.Context, env Envelope) {
	var d SearchReq
	if err := json.Unmarshal(env.Data, &d); err != nil {
		s.push(errEnvelope(env.Seq, "bad_request", "malformed search data"))
		return
	}
	if strings.TrimSpace(d.Query) == "" {
		s.push(errEnvelope(env.Seq, "bad_request", "search needs a query"))
		return
	}
	opts := store.SearchOptions{
		Query: d.Query, Network: d.Network, Target: d.Buffer, Limit: d.Limit,
	}
	if d.Before != nil {
		opts.Before = store.Cursor(*d.Before)
	}
	msgs, err := s.hub.store.Search(ctx, opts)
	if err != nil {
		s.push(errEnvelope(env.Seq, "internal", "search failed"))
		return
	}
	out := SearchData{Query: d.Query, Messages: make([]EventData, 0, len(msgs))}
	for _, m := range msgs {
		out.Messages = append(out.Messages, eventData(m))
	}
	s.push(envelope("search_results", env.Seq, out))
}

func (s *Session) handleGetMarker(ctx context.Context, env Envelope) {
	var d MarkerRef
	if err := json.Unmarshal(env.Data, &d); err != nil {
		s.push(errEnvelope(env.Seq, "bad_request", "malformed get_read_marker data"))
		return
	}
	t, err := s.hub.store.ReadMarker(ctx, d.Network, d.Buffer)
	if err != nil {
		s.push(errEnvelope(env.Seq, "internal", "read marker query failed"))
		return
	}
	s.push(envelope("read_marker", env.Seq, MarkerData{
		Network: d.Network, Buffer: d.Buffer, Time: markerMillis(t),
	}))
}

func (s *Session) handleSetMarker(ctx context.Context, env Envelope) {
	var d SetMarkerData
	if err := json.Unmarshal(env.Data, &d); err != nil {
		s.push(errEnvelope(env.Seq, "bad_request", "malformed set_read_marker data"))
		return
	}
	if d.Network == "" || d.Buffer == "" || d.Time <= 0 {
		s.push(errEnvelope(env.Seq, "bad_request", "set_read_marker needs network, buffer and time"))
		return
	}
	if err := s.hub.store.SetReadMarker(ctx, d.Network, d.Buffer, time.UnixMilli(d.Time)); err != nil {
		s.push(errEnvelope(env.Seq, "internal", "setting read marker failed"))
		return
	}
	// Read back: markers never move backwards, so the stored value is the
	// authoritative one for every device, including a requester that
	// tried to regress it.
	t, err := s.hub.store.ReadMarker(ctx, d.Network, d.Buffer)
	if err != nil {
		s.push(errEnvelope(env.Seq, "internal", "read marker query failed"))
		return
	}
	data := MarkerData{Network: d.Network, Buffer: d.Buffer, Time: markerMillis(t)}
	s.push(envelope("read_marker", env.Seq, data))
	s.hub.broadcastExcept(s, envelope("read_marker", 0, data))
	// Bridge to draft/read-marker: other clients of this account (e.g.
	// on a bouncer) learn our read position. The authoritative value is
	// sent, never a regression.
	if conn := s.hub.network(d.Network); conn != nil && conn.CapEnabled("draft/read-marker") && !t.IsZero() {
		_ = conn.Send(&ircv4.Message{
			Command: "MARKREAD",
			Params:  []string{d.Buffer, "timestamp=" + t.UTC().Format(markreadTimeLayout)},
		})
	}
}

// markreadTimeLayout is the server-time format used by MARKREAD
// (https://ircv3.net/specs/extensions/read-marker, fetched 2026-07-15).
const markreadTimeLayout = "2006-01-02T15:04:05.000Z"

// markerMillis maps the zero time (marker unset) to protocol 0.
func markerMillis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}
