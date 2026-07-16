package irc

import (
	"fmt"
	"io"
)

// maxIncomingLine bounds one server-to-client IRC line: 512 bytes of
// message plus the IRCv3 server tag budget (8191 bytes) with generous
// slack. The irc.v4 reader accumulates until a newline with no cap of
// its own, so without this a malicious server (or an attacker on an
// explicitly allowed plaintext connection) could stream one unterminated
// line and grow memory until the process died — the read deadline never
// fires while bytes keep arriving.
const maxIncomingLine = 16 * 1024

// boundedLineReader passes bytes through while counting the current
// line; a line exceeding max fails the read, which tears the connection
// down (the parser's buffered growth stops at roughly max plus one read
// chunk).
type boundedLineReader struct {
	r   io.Reader
	max int
	run int // bytes seen since the last newline
}

func (b *boundedLineReader) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	for _, c := range p[:n] {
		if c == '\n' {
			b.run = 0
		} else if b.run++; b.run > b.max {
			return n, fmt.Errorf("irc: server sent a line exceeding %d bytes", b.max)
		}
	}
	return n, err
}
