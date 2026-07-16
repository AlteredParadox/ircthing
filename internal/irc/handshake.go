package irc

import (
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	ircv4 "gopkg.in/irc.v4"
)

// wantedCaps is the capability set we negotiate: the ratified IRCv3 set
// plus the draft caps CLAUDE.md treats as required (sasl is appended when
// configured). Caps the server doesn't offer are simply not requested.
// sts is deliberately absent — the spec forbids requesting it; it is
// handled out-of-band at CAP LS (see sts.go).
var wantedCaps = []string{
	"account-notify",
	"account-tag",
	"away-notify",
	"batch",
	"cap-notify",
	"chghost",
	"draft/chathistory",
	"draft/multiline",
	"draft/event-playback",
	"draft/message-redaction",
	"draft/read-marker",
	"echo-message",
	"extended-join",
	"extended-monitor",
	"invite-notify",
	"labeled-response",
	"message-tags",
	"multi-prefix",
	"no-implicit-names",
	"server-time",
	"setname",
	"standard-replies",
	"userhost-in-names",
}

var wantedCapSet = func() map[string]bool {
	m := make(map[string]bool, len(wantedCaps))
	for _, c := range wantedCaps {
		m[c] = true
	}
	return m
}()

// Connection registration: PASS/NICK/USER, capability negotiation, and
// SASL PLAIN. Implements:
//
//   - RFC 2812 §3.1 registration, with fallback nicks on 433/436.
//   - IRCv3 Capability Negotiation, version 302
//     (https://ircv3.net/specs/extensions/capability-negotiation, fetched
//     2026-07-14): CAP LS 302, multiline replies (an "*" parameter before
//     the capability list on all but the last line), capability values
//     (name=value), REQ/ACK/NAK, and registration being suspended until
//     CAP END.
//   - IRCv3.1 SASL (https://ircv3.net/specs/extensions/sasl-3.1, fetched
//     2026-07-14), PLAIN only. CAP END is deferred until the SASL exchange
//     completes, as the spec recommends.
//
// The state machine is pure: callers feed each server message to handle()
// and write the returned messages to the connection. That keeps every
// parsing path table-testable without a socket.

type hsPhase int

const (
	hsCapLS         hsPhase = iota // waiting for the (possibly multiline) CAP LS reply
	hsCapAck                       // sent CAP REQ :sasl, waiting for ACK/NAK
	hsAuthChallenge                // sent AUTHENTICATE PLAIN, waiting for the "+" challenge
	hsAuthResult                   // sent credentials, waiting for 903/904/...
	hsAwaitWelcome                 // sent CAP END, waiting for 001
	hsDone
)

type handshake struct {
	cfg        *Config
	phase      hsPhase
	caps       map[string]string // accumulated across multiline CAP LS
	enabled    map[string]bool   // caps the server ACKed
	nick       string            // current nick, updated by 433/436 fallback and 001
	nickTries  int
	saslDone   bool
	nakRetried bool // one sasl-only retry after a NAK of the full set
	lastReq    []string
	mech       saslMech // chosen SASL mechanism, built at CAP LS
	mechErr    error    // mechanism unavailable; abort after CAP ACK

	// secure is whether this connection runs over TLS; it gates STS
	// handling (see sts.go). stsDuration is set when a secure CAP LS
	// carries a duration policy, for the manager to persist.
	secure      bool
	stsDuration *time.Duration
}

func newHandshake(cfg *Config) *handshake {
	return &handshake{
		cfg:     cfg,
		phase:   hsCapLS,
		caps:    make(map[string]string),
		enabled: make(map[string]bool),
		nick:    cfg.Nick,
	}
}

func newMsg(cmd string, params ...string) *ircv4.Message {
	return &ircv4.Message{Command: cmd, Params: params}
}

// start returns the messages that open registration. Sending CAP LS
// suspends registration server-side until CAP END ("the server MUST not
// complete registration until the client sends a CAP END command"), which
// gives SASL room to run; servers without CAP support ignore it and
// register us directly.
func (h *handshake) start() []*ircv4.Message {
	// PASS is deferred (see passLine): the server password is the one
	// secret in the opening burst, and on an insecure link it must not
	// go out before the CAP LS reply reveals a pending STS upgrade —
	// otherwise a passive eavesdropper captures it before STS aborts the
	// connection to redial over TLS. NICK/USER are public and stay here.
	out := []*ircv4.Message{newMsg("CAP", "LS", "302")}
	username := h.cfg.Username
	if username == "" {
		username = h.cfg.Nick
	}
	realname := h.cfg.Realname
	if realname == "" {
		realname = h.cfg.Nick
	}
	return append(out,
		newMsg("NICK", h.nick),
		newMsg("USER", username, "0", "*", realname),
	)
}

// passLine returns the deferred PASS message (empty when no server
// password is configured), emitted only after the STS upgrade decision.
func (h *handshake) passLine() []*ircv4.Message {
	if h.cfg.Pass == "" {
		return nil
	}
	return []*ircv4.Message{newMsg("PASS", h.cfg.Pass)}
}

// handle processes one server message during registration. It returns the
// messages to send in response and done=true once the server has accepted
// registration (001). A non-nil error aborts the connection attempt.
func (h *handshake) handle(m *ircv4.Message) (out []*ircv4.Message, done bool, err error) {
	switch m.Command {
	case "PING":
		return []*ircv4.Message{newMsg("PONG", m.Params...)}, false, nil

	case "ERROR":
		return nil, false, fmt.Errorf("server error: %s", m.Trailing())

	case "CAP":
		return h.handleCAP(m)

	case "AUTHENTICATE":
		return h.handleAuthenticate(m)

	case "001": // RPL_WELCOME: registration accepted
		if h.cfg.SASL != nil && !h.saslDone {
			// The server registered us before SASL finished (it SHOULD
			// have sent 906). Never fall through to an unauthenticated
			// session when credentials were configured.
			return nil, false, errors.New("registered before SASL authentication completed")
		}
		h.phase = hsDone
		// 001's first param is the nick the server knows us by —
		// authoritative if it truncated or otherwise changed ours.
		if p := m.Param(0); p != "" && p != "*" {
			h.nick = p
		}
		return nil, true, nil

	case "433", "436": // ERR_NICKNAMEINUSE, ERR_NICKCOLLISION
		h.nickTries++
		if h.nickTries > 3 {
			return nil, false, fmt.Errorf("nickname %q and all fallbacks are in use", h.cfg.Nick)
		}
		h.nick = h.cfg.Nick + strings.Repeat("_", h.nickTries)
		return []*ircv4.Message{newMsg("NICK", h.nick)}, false, nil

	case "432": // ERR_ERRONEUSNICKNAME
		return nil, false, fmt.Errorf("server rejected nickname %q", h.nick)

	case "464": // ERR_PASSWDMISMATCH
		return nil, false, errors.New("server password rejected (464)")

	case "900": // RPL_LOGGEDIN — informational; 903 confirms completion
		return nil, false, nil

	case "902": // ERR_NICKLOCKED
		return nil, false, errors.New("SASL: nick locked by services (902)")

	case "903": // RPL_SASLSUCCESS
		h.saslDone = true
		h.phase = hsAwaitWelcome
		return []*ircv4.Message{newMsg("CAP", "END")}, false, nil

	case "904": // ERR_SASLFAIL
		return nil, false, errors.New("SASL authentication failed (904): bad credentials?")

	case "905": // ERR_SASLTOOLONG
		return nil, false, errors.New("SASL: AUTHENTICATE line too long (905)")

	case "906": // ERR_SASLABORTED
		return nil, false, errors.New("SASL: authentication aborted (906)")

	case "907": // ERR_SASLALREADY
		return nil, false, errors.New("SASL: unexpected re-authentication rejection (907)")

	case "908": // RPL_SASLMECHS — informational; a 904 follows
		return nil, false, nil
	}

	// Anything else (020, 042, snotices, ...) is passed through by the
	// caller as a regular event.
	return nil, false, nil
}

func (h *handshake) handleCAP(m *ircv4.Message) ([]*ircv4.Message, bool, error) {
	if len(m.Params) < 2 {
		return nil, false, fmt.Errorf("malformed CAP reply: %q", m.String())
	}
	switch strings.ToUpper(m.Params[1]) {
	case "LS":
		return h.handleCapLS(m)
	case "ACK":
		return h.handleCapACK(m)
	case "NAK":
		return h.handleCapNAK()
	}
	// NEW/DEL/LIST during registration: nothing to do yet.
	return nil, false, nil
}

func (h *handshake) handleCapLS(m *ircv4.Message) ([]*ircv4.Message, bool, error) {
	if h.phase != hsCapLS {
		return nil, false, nil
	}
	// Multiline form: CAP <nick> LS * :<caps> — a lone "*" before the
	// capability list means more lines follow.
	more := len(m.Params) >= 4 && m.Params[2] == "*"
	parseCapList(m.Params[len(m.Params)-1], h.caps)
	if more {
		return nil, false, nil
	}
	// STS (sts.go): on an insecure connection a valid port upgrades the
	// connection (abort, secure redial); on a secure one the duration
	// policy is recorded for the manager to persist. Never requested
	// with CAP REQ.
	if val, ok := h.caps["sts"]; ok {
		v := parseSTS(val)
		if !h.secure && v.port > 0 {
			return nil, false, errSTSUpgrade{port: v.port}
		}
		if h.secure && v.hasDuration {
			d := v.duration
			h.stsDuration = &d
		}
	}
	if err := h.chooseMech(); err != nil {
		return nil, false, err
	}
	// The STS decision is made (no upgrade): now it is safe to send PASS.
	out := h.passLine()
	reqs := h.capsToRequest()
	if len(reqs) == 0 {
		h.phase = hsAwaitWelcome
		return append(out, newMsg("CAP", "END")), false, nil
	}
	h.phase = hsCapAck
	h.lastReq = reqs
	return append(out, newMsg("CAP", "REQ", strings.Join(reqs, " "))), false, nil
}

// chooseMech picks and builds the SASL mechanism from the advertised
// list (empty when the server didn't advertise one under CAP 302). When
// our mechanism isn't offered we still REQ sasl and only quit after the
// ACK — the conventional client flow (and the one irctest asserts);
// either way we never fall through to an unauthenticated session.
func (h *handshake) chooseMech() error {
	if h.cfg.SASL == nil {
		return nil
	}
	mechs, offered := h.caps["sasl"]
	if !offered {
		return errors.New("SASL configured but the server does not offer the sasl capability")
	}
	mech, err := newMech(h.cfg.SASL, mechs)
	if err != nil {
		h.mechErr = err
	}
	h.mech = mech
	return nil
}

func (h *handshake) handleCapACK(m *ircv4.Message) ([]*ircv4.Message, bool, error) {
	if h.phase != hsCapAck {
		return nil, false, nil
	}
	for _, tok := range strings.Fields(m.Params[len(m.Params)-1]) {
		if name, ok := strings.CutPrefix(tok, "-"); ok {
			delete(h.enabled, name)
		} else {
			h.enabled[tok] = true
		}
	}
	if h.cfg.SASL != nil && h.enabled["sasl"] && h.mechErr != nil {
		return []*ircv4.Message{newMsg("QUIT", "SASL mechanism unavailable")}, false, h.mechErr
	}
	if h.mech != nil && h.enabled["sasl"] && !h.saslDone {
		h.phase = hsAuthChallenge
		return []*ircv4.Message{newMsg("AUTHENTICATE", h.mech.Name())}, false, nil
	}
	h.phase = hsAwaitWelcome
	return []*ircv4.Message{newMsg("CAP", "END")}, false, nil
}

func (h *handshake) handleCapNAK() ([]*ircv4.Message, bool, error) {
	if h.phase != hsCapAck {
		return nil, false, nil
	}
	// REQ is all-or-nothing (spec: "accepted as a whole, or rejected
	// entirely"). A NAK of offered caps is abnormal; retry once with
	// just sasl — that one we cannot proceed without.
	if h.cfg.SASL != nil {
		if !h.nakRetried && len(h.lastReq) > 1 {
			h.nakRetried = true
			h.lastReq = []string{"sasl"}
			return []*ircv4.Message{newMsg("CAP", "REQ", "sasl")}, false, nil
		}
		return nil, false, errors.New("server refused the capability request including sasl")
	}
	h.phase = hsAwaitWelcome
	return []*ircv4.Message{newMsg("CAP", "END")}, false, nil
}

func (h *handshake) handleAuthenticate(m *ircv4.Message) ([]*ircv4.Message, bool, error) {
	if h.phase != hsAuthChallenge || h.mech == nil {
		return nil, false, nil
	}
	// A server AUTHENTICATE carries the base64 challenge, or "+" for an
	// empty one. Multi-round mechanisms (SCRAM) reassemble across it.
	challenge, err := decodeChallenge(m.Param(0))
	if err != nil {
		return nil, false, err
	}
	resp, err := h.mech.respond(challenge)
	if err != nil {
		// Abort the exchange; the connection attempt then fails and retries.
		return []*ircv4.Message{newMsg("AUTHENTICATE", "*")}, false, fmt.Errorf("SASL %s: %w", h.mech.Name(), err)
	}
	var out []*ircv4.Message
	for _, chunk := range chunkAuthenticate(base64.StdEncoding.EncodeToString(resp)) {
		out = append(out, newMsg("AUTHENTICATE", chunk))
	}
	return out, false, nil
}

// decodeChallenge base64-decodes a server AUTHENTICATE argument; "+"
// means an empty challenge.
func decodeChallenge(arg string) ([]byte, error) {
	if arg == "+" || arg == "" {
		return nil, nil
	}
	b, err := base64.StdEncoding.DecodeString(arg)
	if err != nil {
		return nil, fmt.Errorf("SASL: undecodable server challenge: %w", err)
	}
	return b, nil
}

// capsToRequest is the sorted intersection of what we want and what the
// server offers.
func (h *handshake) capsToRequest() []string {
	var out []string
	for name := range h.caps {
		if wantedCapSet[name] {
			out = append(out, name)
		}
	}
	if h.cfg.SASL != nil {
		if _, ok := h.caps["sasl"]; ok {
			out = append(out, "sasl")
		}
	}
	sort.Strings(out)
	return out
}

// parseCapList adds the entries of one CAP LS capability list
// ("multi-prefix sasl=PLAIN,EXTERNAL server-time") to dst. Values (after
// "=") are a CAP 302 feature and default to "".
// maxAdvertisedCaps bounds the CAP LS accumulation against a server that
// streams unbounded '*'-continued capability lines. Real servers
// advertise a few dozen.
const maxAdvertisedCaps = 512

func parseCapList(list string, dst map[string]string) {
	for _, tok := range strings.Fields(list) {
		name, val, _ := strings.Cut(tok, "=")
		if _, known := dst[name]; !known && len(dst) >= maxAdvertisedCaps {
			continue // bound the map; ignore further new caps
		}
		dst[name] = val
	}
}

// mechListed reports whether mech occurs in a comma-separated mechanism
// list such as the sasl capability value "PLAIN,EXTERNAL".
func mechListed(list, mech string) bool {
	for _, m := range strings.Split(list, ",") {
		if strings.EqualFold(m, mech) {
			return true
		}
	}
	return false
}
