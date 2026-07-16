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

func TestBoundedLineReader(t *testing.T) {
	// Normal traffic passes through untouched.
	src := ":irc.test 001 me :hi\r\n:irc.test PING :x\r\n"
	br := bufio.NewReader(&boundedLineReader{r: strings.NewReader(src), max: 64})
	for i := 0; i < 2; i++ {
		if _, err := br.ReadString('\n'); err != nil {
			t.Fatalf("line %d: %v", i, err)
		}
	}

	// Long-but-legal line right at the cap survives; the newline resets
	// the counter for the next line.
	line := strings.Repeat("x", 60) + "\n" + strings.Repeat("y", 60) + "\n"
	br = bufio.NewReader(&boundedLineReader{r: strings.NewReader(line), max: 64})
	for i := 0; i < 2; i++ {
		if _, err := br.ReadString('\n'); err != nil {
			t.Fatalf("capped line %d: %v", i, err)
		}
	}

	// An unterminated flood errors instead of growing without bound.
	br = bufio.NewReader(&boundedLineReader{r: endless{}, max: 1024})
	_, err := br.ReadString('\n')
	if err == nil || err == io.EOF || !strings.Contains(err.Error(), "exceeding") {
		t.Fatalf("flood err = %v, want line-length error", err)
	}
}
