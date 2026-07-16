package irc

import (
	"bufio"
	"io"
	"strings"
	"testing"
)

// endless emits 'a' forever — an unterminated line.
type endless struct{}

func (endless) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'a'
	}
	return len(p), nil
}

func newTestReader(r io.Reader, max int) *boundedLineReader {
	b := newBoundedLineReader(r)
	b.setLimit(max)
	return b
}

func TestBoundedLineReader(t *testing.T) {
	// Normal traffic passes through untouched.
	src := ":irc.test 001 me :hi\r\n:irc.test PING :x\r\n"
	br := bufio.NewReader(newBoundedLineReader(strings.NewReader(src)))
	for i := 0; i < 2; i++ {
		if _, err := br.ReadString('\n'); err != nil {
			t.Fatalf("line %d: %v", i, err)
		}
	}

	// A run beyond the default cap (excess large enough that the overflow
	// lands in a delimiter-free read) is rejected; the same run under a
	// LINELEN-raised limit is accepted. The security property is about
	// unterminated growth — the flood case below is the direct check.
	over := strings.Repeat("x", defaultIncomingLine+8192) + "\n"
	br = bufio.NewReader(newBoundedLineReader(strings.NewReader(over)))
	if _, err := br.ReadString('\n'); err == nil || !strings.Contains(err.Error(), "exceeding") {
		t.Fatalf("over-default line err = %v, want rejection", err)
	}
	raised := newTestReader(strings.NewReader(over), defaultIncomingLine+16384)
	br = bufio.NewReader(raised)
	if _, err := br.ReadString('\n'); err != nil {
		t.Fatalf("raised-limit line: %v", err)
	}

	// The ceiling still bounds an absurd advertised value.
	huge := newTestReader(endless{}, 1<<30)
	if int(huge.max.Load()) != maxIncomingLineCeiling {
		t.Fatalf("limit = %d, want clamp to ceiling %d", huge.max.Load(), maxIncomingLineCeiling)
	}

	// An unterminated flood errors instead of growing without bound.
	br = bufio.NewReader(newTestReader(endless{}, 1024))
	_, err := br.ReadString('\n')
	if err == nil || err == io.EOF || !strings.Contains(err.Error(), "exceeding") {
		t.Fatalf("flood err = %v, want line-length error", err)
	}
}
