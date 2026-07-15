package hub

import "encoding/json"

// Client sync protocol (browser <-> server WebSocket): JSON messages in a
// versioned envelope.
//
// Forward-compatibility rules (CLAUDE.md):
//   - Unknown message types MUST be ignored, not errored, on both ends.
//   - Envelopes with an unknown version are ignored.
//   - Read markers and history paging are first-class message types from
//     day one so Phase 2 extends the protocol rather than breaking it.
//
// Server -> client types: "event", "state", "history", "read_marker",
// "ok", "error".
// Client -> server types: "send", "get_history", "get_read_marker",
// "set_read_marker".
//
// Seq is a client-chosen request id; responses (and errors) echo it.
// Server pushes (events, state, foreign read-marker updates) carry no Seq.

const ProtocolVersion = 1

type Envelope struct {
	V    int             `json:"v"`
	Type string          `json:"type"`
	Seq  int64           `json:"seq,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

// EventData is one message in a buffer — both live pushes ("event") and
// the elements of a history page. Times are unix milliseconds everywhere
// in the protocol.
type EventData struct {
	Network string `json:"network"`
	Buffer  string `json:"buffer"`
	ID      int64  `json:"id"`
	Time    int64  `json:"time"`
	MsgID   string `json:"msgid,omitempty"`
	Sender  string `json:"sender"`
	Command string `json:"command"`
	Raw     string `json:"raw"`
}

// StateData reports a network connection state change ("state").
type StateData struct {
	Network string `json:"network"`
	State   string `json:"state"`
	Error   string `json:"error,omitempty"`
}

// Cursor is a history position, matching store.Cursor.
type Cursor struct {
	TS int64 `json:"ts"`
	ID int64 `json:"id"`
}

// HistoryReq asks for a page of history ("get_history"). At most one
// anchor may be set; none means "the newest page". Limit is clamped
// server-side.
type HistoryReq struct {
	Network     string  `json:"network"`
	Buffer      string  `json:"buffer"`
	Before      *Cursor `json:"before,omitempty"`
	After       *Cursor `json:"after,omitempty"`
	BeforeMsgID string  `json:"before_msgid,omitempty"`
	AfterMsgID  string  `json:"after_msgid,omitempty"`
	Limit       int     `json:"limit,omitempty"`
}

// HistoryData is the "history" response: messages in ascending time order.
type HistoryData struct {
	Network  string      `json:"network"`
	Buffer   string      `json:"buffer"`
	Messages []EventData `json:"messages"`
}

// MarkerRef names a buffer's read marker ("get_read_marker").
type MarkerRef struct {
	Network string `json:"network"`
	Buffer  string `json:"buffer"`
}

// SetMarkerData advances a read marker ("set_read_marker").
type SetMarkerData struct {
	Network string `json:"network"`
	Buffer  string `json:"buffer"`
	Time    int64  `json:"time"`
}

// MarkerData is the "read_marker" response and cross-device push. The
// value is authoritative: it may be newer than what a set requested,
// because markers never move backwards. 0 means unset.
type MarkerData struct {
	Network string `json:"network"`
	Buffer  string `json:"buffer"`
	Time    int64  `json:"time"`
}

// NetworkInfo describes one configured network connection ("buffers").
type NetworkInfo struct {
	Name  string `json:"name"`
	State string `json:"state"`
	Nick  string `json:"nick"`
}

// BufferInfo describes one buffer for the sidebar ("buffers").
type BufferInfo struct {
	Network  string `json:"network"`
	Buffer   string `json:"buffer"`
	LastTime int64  `json:"last_time"` // unix ms of newest message, 0 if none
	Marker   int64  `json:"marker"`    // unix ms read marker, 0 if unset
	Unread   int64  `json:"unread"`    // messages newer than the marker
}

// BuffersData answers "get_buffers" (which carries no request data): the
// initial state a client needs to draw its sidebar.
type BuffersData struct {
	Networks []NetworkInfo `json:"networks"`
	Buffers  []BufferInfo  `json:"buffers"`
}

// SendData submits an outgoing message ("send"). Newlines split it into
// one PRIVMSG per line.
type SendData struct {
	Network string `json:"network"`
	Target  string `json:"target"`
	Text    string `json:"text"`
}

// CommandData submits a structured IRC command ("command"). The server
// accepts only an allowlist (JOIN, PART, NICK, TOPIC) and validates
// params; everything else is rejected rather than passed through.
type CommandData struct {
	Network string   `json:"network"`
	Command string   `json:"command"`
	Params  []string `json:"params"`
}

// ChannelReq asks for a channel's live state ("get_channel").
type ChannelReq struct {
	Network string `json:"network"`
	Buffer  string `json:"buffer"`
}

// MemberData is one channel occupant.
type MemberData struct {
	Nick   string `json:"nick"`
	Prefix string `json:"prefix,omitempty"` // "~", "&", "@", "%", "+" or ""
}

// ChannelData answers "get_channel": topic and membership as currently
// known. Joined is false for channels we are not in (PM buffers,
// disconnected networks) — members and topic are then empty.
type ChannelData struct {
	Network string       `json:"network"`
	Buffer  string       `json:"buffer"`
	Joined  bool         `json:"joined"`
	Topic   string       `json:"topic"`
	Members []MemberData `json:"members"`
}

// MembersChangedData is a server push hinting that channel state changed
// and interested clients should refetch ("members_changed"). An empty
// Buffer means anywhere on the network (QUIT/NICK span channels).
type MembersChangedData struct {
	Network string `json:"network"`
	Buffer  string `json:"buffer"`
}

// ErrorData is the "error" response.
type ErrorData struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// envelope marshals a payload into a versioned envelope. Payloads are our
// own types; marshaling them cannot fail.
func envelope(typ string, seq int64, data any) Envelope {
	var raw json.RawMessage
	if data != nil {
		raw, _ = json.Marshal(data)
	}
	return Envelope{V: ProtocolVersion, Type: typ, Seq: seq, Data: raw}
}

func errEnvelope(seq int64, code, msg string) Envelope {
	return envelope("error", seq, ErrorData{Code: code, Message: msg})
}
