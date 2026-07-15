package irc

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	ircv4 "gopkg.in/irc.v4"
)

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
	cfg       *Config
	phase     hsPhase
	caps      map[string]string // accumulated across multiline CAP LS
	nick      string            // current nick, updated by 433/436 fallback and 001
	nickTries int
	saslDone  bool
}

func newHandshake(cfg *Config) *handshake {
	return &handshake{cfg: cfg, phase: hsCapLS, caps: make(map[string]string), nick: cfg.Nick}
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
	out := []*ircv4.Message{newMsg("CAP", "LS", "302")}
	if h.cfg.Pass != "" {
		out = append(out, newMsg("PASS", h.cfg.Pass))
	}
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
		if h.cfg.SASL == nil {
			h.phase = hsAwaitWelcome
			return []*ircv4.Message{newMsg("CAP", "END")}, false, nil
		}
		mechs, offered := h.caps["sasl"]
		if !offered {
			return nil, false, errors.New("SASL configured but the server does not offer the sasl capability")
		}
		// With CAP 302 the sasl value lists mechanisms; an empty value
		// means the server didn't say, so try PLAIN anyway.
		if mechs != "" && !mechListed(mechs, "PLAIN") {
			return nil, false, fmt.Errorf("SASL PLAIN not offered (server mechanisms: %s)", mechs)
		}
		h.phase = hsCapAck
		return []*ircv4.Message{newMsg("CAP", "REQ", "sasl")}, false, nil

	case "ACK":
		if h.phase != hsCapAck {
			return nil, false, nil
		}
		h.phase = hsAuthChallenge
		return []*ircv4.Message{newMsg("AUTHENTICATE", "PLAIN")}, false, nil

	case "NAK":
		if h.phase == hsCapAck {
			return nil, false, errors.New("server refused CAP REQ sasl")
		}
		return nil, false, nil
	}

	// NEW/DEL/LIST during registration: nothing to do yet.
	return nil, false, nil
}

func (h *handshake) handleAuthenticate(m *ircv4.Message) ([]*ircv4.Message, bool, error) {
	if h.phase != hsAuthChallenge {
		return nil, false, nil
	}
	// PLAIN is a single-round mechanism: the only valid server challenge
	// is the empty one ("+").
	if m.Param(0) != "+" {
		return nil, false, fmt.Errorf("unexpected SASL challenge %q for PLAIN", m.Param(0))
	}
	blob := saslPlain("", h.cfg.SASL.Login, h.cfg.SASL.Password)
	var out []*ircv4.Message
	for _, chunk := range chunkAuthenticate(base64.StdEncoding.EncodeToString(blob)) {
		out = append(out, newMsg("AUTHENTICATE", chunk))
	}
	h.phase = hsAuthResult
	return out, false, nil
}

// parseCapList adds the entries of one CAP LS capability list
// ("multi-prefix sasl=PLAIN,EXTERNAL server-time") to dst. Values (after
// "=") are a CAP 302 feature and default to "".
func parseCapList(list string, dst map[string]string) {
	for _, tok := range strings.Fields(list) {
		name, val, _ := strings.Cut(tok, "=")
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
