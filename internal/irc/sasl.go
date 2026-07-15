package irc

import (
	"fmt"
	"strings"
)

// SASL mechanisms carried over the IRCv3.1 SASL extension
// (https://ircv3.net/specs/extensions/sasl-3.1, fetched 2026-07-14):
// PLAIN (RFC 4616), EXTERNAL (RFC 4422, TLS client certificate), and
// SCRAM-SHA-256 (see scram.go).

// saslMech is a client SASL mechanism. respond is called for each server
// AUTHENTICATE challenge (challenge is the decoded server data, "" for
// the initial empty "+" prompt) and returns the raw response to send.
// Single-round mechanisms return their whole response on the first call.
type saslMech interface {
	Name() string
	respond(challenge []byte) ([]byte, error)
}

// plainMech is SASL PLAIN (RFC 4616): a one-shot [authzid] NUL authcid
// NUL passwd payload.
type plainMech struct {
	authzid, authcid, passwd string
	sent                     bool
}

func (m *plainMech) Name() string { return "PLAIN" }

func (m *plainMech) respond(_ []byte) ([]byte, error) {
	if m.sent {
		return nil, fmt.Errorf("PLAIN: unexpected extra challenge")
	}
	m.sent = true
	return saslPlain(m.authzid, m.authcid, m.passwd), nil
}

// externalMech is SASL EXTERNAL (RFC 4422 §A): authentication is proven
// by the TLS client certificate, so the response is just the (usually
// empty) authorization identity.
type externalMech struct {
	authzid string
	sent    bool
}

func (m *externalMech) Name() string { return "EXTERNAL" }

func (m *externalMech) respond(_ []byte) ([]byte, error) {
	if m.sent {
		return nil, fmt.Errorf("EXTERNAL: unexpected extra challenge")
	}
	m.sent = true
	return []byte(m.authzid), nil
}

// newMech builds the client mechanism for the configured SASL settings,
// choosing automatically when Mechanism is empty: EXTERNAL if no password
// is set (cert-based), otherwise SCRAM-SHA-256 when the server offers it,
// falling back to PLAIN. offered is the server's advertised mechanism
// list (comma-separated), possibly empty.
func newMech(cfg *SASLConfig, offered string) (saslMech, error) {
	mech := strings.ToUpper(cfg.Mechanism)
	if mech == "" {
		switch {
		case cfg.Password == "":
			mech = "EXTERNAL"
		case offered == "" || mechListed(offered, "SCRAM-SHA-256"):
			mech = "SCRAM-SHA-256"
		default:
			mech = "PLAIN"
		}
	}
	if offered != "" && !mechListed(offered, mech) {
		return nil, fmt.Errorf("SASL %s not offered (server mechanisms: %s)", mech, offered)
	}
	switch mech {
	case "PLAIN":
		// Default the authorization identity to the login. RFC 4616 allows
		// leaving it empty, but authzid == authcid is what established IRC
		// clients send and what irctest asserts; services treat them alike.
		authzid := cfg.Authzid
		if authzid == "" {
			authzid = cfg.Login
		}
		return &plainMech{authzid: authzid, authcid: cfg.Login, passwd: cfg.Password}, nil
	case "EXTERNAL":
		return &externalMech{authzid: cfg.Authzid}, nil
	case "SCRAM-SHA-256":
		return newSCRAM(cfg.Authzid, cfg.Login, cfg.Password), nil
	default:
		return nil, fmt.Errorf("unsupported SASL mechanism %q", cfg.Mechanism)
	}
}

// saslPlain builds the PLAIN initial response (RFC 4616 §2):
//
//	message = [authzid] UTF8NUL authcid UTF8NUL passwd
func saslPlain(authzid, authcid, passwd string) []byte {
	b := make([]byte, 0, len(authzid)+len(authcid)+len(passwd)+2)
	b = append(b, authzid...)
	b = append(b, 0)
	b = append(b, authcid...)
	b = append(b, 0)
	b = append(b, passwd...)
	return b
}

// chunkAuthenticate splits a base64-encoded SASL response into the
// arguments of consecutive AUTHENTICATE commands. sasl-3.1: "The response
// is encoded in Base64, then split to 400-byte chunks"; "If the last chunk
// was exactly 400 bytes long, it must also be followed by AUTHENTICATE +";
// an empty response is a single "+".
func chunkAuthenticate(b64 string) []string {
	if b64 == "" {
		return []string{"+"}
	}
	var chunks []string
	for len(b64) > 400 {
		chunks = append(chunks, b64[:400])
		b64 = b64[400:]
	}
	chunks = append(chunks, b64)
	if len(b64) == 400 {
		chunks = append(chunks, "+")
	}
	return chunks
}
