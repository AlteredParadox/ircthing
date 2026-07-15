package irc

// SASL PLAIN payload construction, per RFC 4616 (the PLAIN mechanism)
// transported per the IRCv3.1 SASL extension
// (https://ircv3.net/specs/extensions/sasl-3.1, fetched 2026-07-14).

// saslPlain builds the PLAIN initial response (RFC 4616 §2):
//
//	message = [authzid] UTF8NUL authcid UTF8NUL passwd
//
// We always send an empty authzid (act as the authenticated identity).
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
