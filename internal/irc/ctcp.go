package irc

import (
	"strings"
	"time"

	ircv4 "gopkg.in/irc.v4"
)

// CTCP query auto-replies (https://modern.ircdocs.horse/ctcp, fetched
// 2026-07-16): VERSION, PING, TIME and CLIENTINFO, per the CLAUDE.md
// scope. ACTION renders as a message and DCC is out of scope. Replies go
// only to queries carried in a PRIVMSG addressed directly to us — never
// to channel-wide CTCP (reply floods) and never to NOTICEs (replies must
// not trigger replies); the caller enforces the addressing, this file
// the content.

// ctcpVersion is the VERSION reply; also listed by CLIENTINFO.
const ctcpVersion = "ircthing"

// ctcpReply returns the NOTICE answering a CTCP query in msg, or nil
// when msg carries none (or one we do not answer). The caller has
// already checked that msg is a PRIVMSG addressed to us and not a
// history replay.
func ctcpReply(msg *ircv4.Message) *ircv4.Message {
	if msg.Prefix == nil || msg.Prefix.Name == "" || len(msg.Params) < 2 {
		return nil
	}
	body := msg.Trailing()
	// A CTCP query is \x01-delimited; the closing delimiter is optional
	// in the wild.
	if !strings.HasPrefix(body, "\x01") {
		return nil
	}
	body = strings.TrimSuffix(strings.TrimPrefix(body, "\x01"), "\x01")
	cmd, args, _ := strings.Cut(body, " ")

	var reply string
	switch strings.ToUpper(cmd) {
	case "VERSION":
		reply = "VERSION " + ctcpVersion
	case "PING":
		// Echo the token back verbatim; the sender computes the latency.
		reply = strings.TrimSpace("PING " + args)
	case "TIME":
		reply = "TIME " + time.Now().Format(time.RFC1123)
	case "CLIENTINFO":
		reply = "CLIENTINFO ACTION CLIENTINFO PING TIME VERSION"
	default: // ACTION, DCC, unknown: no reply
		return nil
	}
	return &ircv4.Message{
		Command: "NOTICE",
		Params:  []string{msg.Prefix.Name, "\x01" + reply + "\x01"},
	}
}
