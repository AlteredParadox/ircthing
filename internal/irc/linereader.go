package irc

import (
	"fmt"
	"io"
	"sync/atomic"
)

// Incoming line limits. The default caps one server-to-client line at
// 512 bytes of message plus the IRCv3 server tag budget (8191) with
// slack; the irc.v4 reader otherwise accumulates until a newline with no
// cap, so a malicious server (or an attacker on an explicitly allowed
// plaintext connection) could stream one unterminated line and grow
// memory until the process died — the read deadline never fires while
// bytes keep arriving. maxIncomingLineCeiling is a hard defensive
// ceiling a negotiated LINELEN may raise the active limit up to, but
// never past.
const (
	defaultIncomingLine    = 16 * 1024
	maxIncomingLineCeiling = 64 * 1024
	// maxTagBytes is the IRCv3 message-tag budget that rides on top of a
	// line's LINELEN (client tag budget is 4094; servers get more —
	// 8191 is the conventional server-tag ceiling). Added to the reader
	// limit so a tagged line at the advertised LINELEN is not cut off.
	maxTagBytes = 8191
)

// boundedLineReader passes bytes through while counting the current
// line; a line exceeding the active limit fails the read, which tears
// the connection down (the parser's buffered growth stops at roughly the
// limit plus one read chunk). The limit is read atomically so the read
// loop can raise it (within the ceiling) after a LINELEN advertisement.
type boundedLineReader struct {
	r   io.Reader
	max atomic.Int64
	run int // bytes seen since the last newline
}

func newBoundedLineReader(r io.Reader) *boundedLineReader {
	b := &boundedLineReader{r: r}
	b.max.Store(defaultIncomingLine)
	return b
}

// setLimit raises (or lowers) the active per-line limit to want, clamped
// to [defaultIncomingLine, maxIncomingLineCeiling]. A server advertising
// a large LINELEN may legally send longer non-tag content, but never
// past the hard ceiling.
func (b *boundedLineReader) setLimit(want int) {
	switch {
	case want < defaultIncomingLine:
		want = defaultIncomingLine
	case want > maxIncomingLineCeiling:
		want = maxIncomingLineCeiling
	}
	b.max.Store(int64(want))
}

// midLine reports whether bytes of the current line have been read since
// the last newline — i.e. a partial line is in progress. Read-loop only.
func (b *boundedLineReader) midLine() bool { return b.run > 0 }

func (b *boundedLineReader) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	max := int(b.max.Load())
	for _, c := range p[:n] {
		if c == '\n' {
			b.run = 0
		} else if b.run++; b.run > max {
			return n, fmt.Errorf("irc: server sent a line exceeding %d bytes", max)
		}
	}
	return n, err
}
