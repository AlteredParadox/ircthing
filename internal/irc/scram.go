package irc

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/pbkdf2"
)

// SCRAM-SHA-256 client (RFC 5802 + RFC 7677) as a SASL mechanism, carried
// over the IRCv3 SASL AUTHENTICATE exchange. Channel binding is not used
// ("n,," GS2 header); the password is not SASLprep-normalised beyond the
// mandatory ',' and '=' escaping in the username — sufficient for the
// ASCII credentials in practice, and noted here as a known limitation.

// scramClient steps through the SCRAM message flow, one server challenge
// at a time. nonce is injectable so tests can use RFC vectors.
type scramClient struct {
	authcid  string
	authzid  string
	password string
	nonce    func() string

	clientFirstBare string
	clientNonce     string
	serverSig       []byte
	step            int
	verified        bool
}


// PBKDF2 iteration bounds for the server-supplied SCRAM i= parameter.
// The minimum is the RFC 5802 baseline (weaker hardening below it); the
// maximum caps how long an attacker-chosen count can pin the goroutine
// in PBKDF2, which the handshake timeout cannot interrupt once started.
const (
	scramMinIters = 4096
	scramMaxIters = 1_000_000
)

func newSCRAM(authzid, authcid, password string) *scramClient {
	return &scramClient{
		authcid:  authcid,
		authzid:  authzid,
		password: password,
		nonce:    randomNonce,
	}
}

func (c *scramClient) Name() string { return "SCRAM-SHA-256" }

// respond advances the exchange: challenge is the decoded server data
// ("" for the initial empty prompt), the return value is the raw response
// to send.
func (c *scramClient) respond(challenge []byte) ([]byte, error) {
	switch c.step {
	case 0:
		c.step++
		return c.clientFirst()
	case 1:
		c.step++
		return c.clientFinal(string(challenge))
	case 2:
		c.step++
		return c.verify(string(challenge))
	default:
		return nil, errors.New("scram: unexpected extra challenge")
	}
}

func (c *scramClient) clientFirst() ([]byte, error) {
	c.clientNonce = c.nonce()
	gs2 := "n," + gs2Authzid(c.authzid) + ","
	c.clientFirstBare = "n=" + scramEscape(c.authcid) + ",r=" + c.clientNonce
	return []byte(gs2 + c.clientFirstBare), nil
}

func (c *scramClient) clientFinal(serverFirst string) ([]byte, error) {
	attrs, err := parseScramAttrs(serverFirst)
	if err != nil {
		return nil, err
	}
	rnonce, salt64, iterStr := attrs["r"], attrs["s"], attrs["i"]
	if rnonce == "" || salt64 == "" || iterStr == "" {
		return nil, fmt.Errorf("scram: incomplete server-first %q", serverFirst)
	}
	// The server nonce MUST extend our client nonce.
	if !strings.HasPrefix(rnonce, c.clientNonce) {
		return nil, errors.New("scram: server nonce does not extend the client nonce")
	}
	salt, err := base64.StdEncoding.DecodeString(salt64)
	if err != nil {
		return nil, fmt.Errorf("scram: bad salt: %w", err)
	}
	iters, err := strconv.Atoi(iterStr)
	if err != nil || iters <= 0 {
		return nil, fmt.Errorf("scram: bad iteration count %q", iterStr)
	}
	// Bound the server-supplied PBKDF2 cost. The count is attacker-
	// controlled (a hostile server, or an active attacker on an
	// explicitly-plaintext connection): below the RFC 5802 baseline it
	// weakens password hardening, and a huge value would pin this
	// goroutine in an uninterruptible PBKDF2 run past the handshake
	// timeout. scramMaxIters bounds latency well above any real server.
	if iters < scramMinIters {
		return nil, fmt.Errorf("scram: server iteration count %d below the minimum %d", iters, scramMinIters)
	}
	if iters > scramMaxIters {
		return nil, fmt.Errorf("scram: server iteration count %d exceeds the maximum %d", iters, scramMaxIters)
	}

	saltedPassword := pbkdf2.Key([]byte(c.password), salt, iters, sha256.Size, sha256.New)
	clientKey := scramHMAC(saltedPassword, []byte("Client Key"))
	storedKey := sha256.Sum256(clientKey)
	serverKey := scramHMAC(saltedPassword, []byte("Server Key"))

	// GS2 header "n,,"" base64-encoded is "biws".
	channelBinding := base64.StdEncoding.EncodeToString([]byte("n," + gs2Authzid(c.authzid) + ","))
	clientFinalNoProof := "c=" + channelBinding + ",r=" + rnonce
	authMessage := c.clientFirstBare + "," + serverFirst + "," + clientFinalNoProof

	clientSig := scramHMAC(storedKey[:], []byte(authMessage))
	proof := xorBytes(clientKey, clientSig)
	c.serverSig = scramHMAC(serverKey, []byte(authMessage))

	final := clientFinalNoProof + ",p=" + base64.StdEncoding.EncodeToString(proof)
	return []byte(final), nil
}

func (c *scramClient) verify(serverFinal string) ([]byte, error) {
	attrs, err := parseScramAttrs(serverFinal)
	if err != nil {
		return nil, err
	}
	if e := attrs["e"]; e != "" {
		return nil, fmt.Errorf("scram: server error: %s", e)
	}
	got, err := base64.StdEncoding.DecodeString(attrs["v"])
	if err != nil {
		return nil, fmt.Errorf("scram: bad server signature: %w", err)
	}
	if subtle.ConstantTimeCompare(got, c.serverSig) != 1 {
		return nil, errors.New("scram: server signature mismatch (possible MITM)")
	}
	// Authenticated: the server proved knowledge of the password.
	c.verified = true
	// Reply with an empty message to complete the exchange.
	return []byte{}, nil
}

func (c *scramClient) completed() bool { return c.verified }

func scramHMAC(key, msg []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(msg)
	return h.Sum(nil)
}

func xorBytes(a, b []byte) []byte {
	out := make([]byte, len(a))
	for i := range a {
		out[i] = a[i] ^ b[i]
	}
	return out
}

func randomNonce() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic("scram: entropy failure: " + err.Error())
	}
	// Base64 without '=' padding keeps the nonce in the printable SCRAM
	// character set and free of ',' / '='.
	return base64.RawStdEncoding.EncodeToString(b)
}

// gs2Authzid renders the optional authorization identity for the GS2
// header (empty when none); the value is escaped per RFC 5801.
func gs2Authzid(authzid string) string {
	if authzid == "" {
		return ""
	}
	r := strings.NewReplacer("=", "=3D", ",", "=2C")
	return "a=" + r.Replace(authzid)
}

// scramEscape encodes the SCRAM username, where ',' and '=' are reserved.
func scramEscape(s string) string {
	return strings.NewReplacer("=", "=3D", ",", "=2C").Replace(s)
}

// parseScramAttrs splits a SCRAM message ("k=v,k=v,...") into its
// attributes.
func parseScramAttrs(msg string) (map[string]string, error) {
	out := make(map[string]string)
	for _, part := range strings.Split(msg, ",") {
		k, v, ok := strings.Cut(part, "=")
		if !ok || len(k) != 1 {
			return nil, fmt.Errorf("scram: malformed attribute %q", part)
		}
		out[k] = v
	}
	return out, nil
}
