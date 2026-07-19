// ircthing — a self-hosted, always-connected web IRC client.
// Copyright (C) 2026 AlteredParadox
//
// This program is free software: you can redistribute it and/or modify it
// under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or (at your
// option) any later version.
//
// This program is distributed in the hope that it will be useful, but WITHOUT
// ANY WARRANTY; without even the implied warranty of MERCHANTABILITY or
// FITNESS FOR A PARTICULAR PURPOSE. See the GNU Affero General Public License
// for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program. If not, see <https://www.gnu.org/licenses/>.

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

// A tagged line at a large advertised LINELEN is accepted: the reader
// limit includes the message-tag budget on top of LINELEN.
func TestLineReaderTagBudget(t *testing.T) {
	// Simulate the manager's setLimit(lineLen + maxTagBytes) for a big
	// advertised LINELEN, then feed a line at that LINELEN plus tags.
	lineLen := 16384
	br := newBoundedLineReader(strings.NewReader(""))
	br.setLimit(lineLen + maxTagBytes)
	if got := int(br.max.Load()); got < lineLen+4000 {
		t.Fatalf("limit = %d, want room for LINELEN + tags", got)
	}
	// A line of LINELEN message bytes plus ~6 KB of tags fits.
	line := "@" + strings.Repeat("t", 6000) + " " + strings.Repeat("x", lineLen) + "\n"
	r := newBoundedLineReader(strings.NewReader(line))
	r.setLimit(lineLen + maxTagBytes)
	rd := bufio.NewReader(r)
	if _, err := rd.ReadString('\n'); err != nil {
		t.Fatalf("tagged line at LINELEN rejected: %v", err)
	}
}

// midLine reflects whether a partial line is in progress, so the read
// loop can distinguish an idle-at-boundary timeout (keepalive) from a
// mid-line stall (tear down).
func TestBoundedLineReaderMidLine(t *testing.T) {
	br := newBoundedLineReader(strings.NewReader("PARTIAL"))
	buf := make([]byte, 16)
	if _, err := br.Read(buf); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if !br.midLine() {
		t.Fatal("midLine false after reading a partial (no newline) line")
	}
	// A full line resets the boundary.
	br2 := newBoundedLineReader(strings.NewReader("FULL\n"))
	br2.Read(buf)
	if br2.midLine() {
		t.Fatal("midLine true after a completed line")
	}
}
