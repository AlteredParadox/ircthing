package irc

import (
	"reflect"
	"testing"

	ircv4 "gopkg.in/irc.v4"
)

// runBatch feeds a script of lines through a multiline collector and
// returns any reconstructed messages plus which inputs were consumed.
func runBatch(t *testing.T, lines ...string) (emitted []*ircv4.Message, consumedCount int) {
	t.Helper()
	ml := newMultiline()
	for _, l := range lines {
		emit, consumed := ml.feed(ircv4.MustParseMessage(l))
		if consumed {
			consumedCount++
		}
		if emit != nil {
			emitted = append(emitted, emit)
		}
	}
	return emitted, consumedCount
}

func TestMultilineReconstruct(t *testing.T) {
	t.Run("newline-joined", func(t *testing.T) {
		emit, consumed := runBatch(t,
			"@msgid=m1;time=2026-07-15T00:00:00.000Z :alice!u@h BATCH +xyz draft/multiline #go",
			"@batch=xyz :alice!u@h PRIVMSG #go :first line",
			"@batch=xyz :alice!u@h PRIVMSG #go :second line",
			":alice!u@h BATCH -xyz",
		)
		if consumed != 4 {
			t.Fatalf("consumed %d of 4", consumed)
		}
		if len(emit) != 1 {
			t.Fatalf("emitted %d messages", len(emit))
		}
		m := emit[0]
		if m.Command != "PRIVMSG" || m.Param(0) != "#go" {
			t.Fatalf("reconstructed = %q", m.String())
		}
		if m.Trailing() != "first line\nsecond line" {
			t.Fatalf("body = %q", m.Trailing())
		}
		if m.Tags["msgid"] != "m1" || m.Prefix.Name != "alice" {
			t.Fatalf("lost msgid/prefix: %q", m.String())
		}
	})

	t.Run("concat tag joins without newline", func(t *testing.T) {
		emit, _ := runBatch(t,
			":a!u@h BATCH +c draft/multiline #go",
			"@batch=c :a!u@h PRIVMSG #go :hello ",
			"@batch=c;draft/multiline-concat :a!u@h PRIVMSG #go :world",
			":a!u@h BATCH -c",
		)
		if len(emit) != 1 || emit[0].Trailing() != "hello world" {
			t.Fatalf("concat body = %q", emit[0].Trailing())
		}
	})

	t.Run("notice batch keeps command", func(t *testing.T) {
		emit, _ := runBatch(t,
			":a!u@h BATCH +n draft/multiline #go",
			"@batch=n :a!u@h NOTICE #go :one",
			"@batch=n :a!u@h NOTICE #go :two",
			":a!u@h BATCH -n",
		)
		if emit[0].Command != "NOTICE" || emit[0].Trailing() != "one\ntwo" {
			t.Fatalf("notice = %q", emit[0].String())
		}
	})

	t.Run("single-line batch", func(t *testing.T) {
		emit, _ := runBatch(t,
			":a!u@h BATCH +s draft/multiline #go",
			"@batch=s :a!u@h PRIVMSG #go :solo",
			":a!u@h BATCH -s",
		)
		if len(emit) != 1 || emit[0].Trailing() != "solo" {
			t.Fatalf("solo = %q", emit[0].Trailing())
		}
	})

	t.Run("non-multiline batch is not consumed", func(t *testing.T) {
		_, consumed := runBatch(t,
			":srv BATCH +h chathistory #go",
			":srv BATCH -h",
		)
		if consumed != 0 {
			t.Fatalf("chathistory batch consumed %d lines", consumed)
		}
	})

	t.Run("empty batch emits nothing", func(t *testing.T) {
		emit, _ := runBatch(t,
			":a!u@h BATCH +e draft/multiline #go",
			":a!u@h BATCH -e",
		)
		if len(emit) != 0 {
			t.Fatalf("empty batch emitted %d", len(emit))
		}
	})

	t.Run("untagged messages pass through", func(t *testing.T) {
		ml := newMultiline()
		_, consumed := ml.feed(ircv4.MustParseMessage(":a!u@h PRIVMSG #go :normal"))
		if consumed {
			t.Fatal("normal message consumed")
		}
	})
}

func TestBuildMultilineBatch(t *testing.T) {
	msgs := buildMultilineBatch("r1", "#go", []string{"a", "b", "c"}, multilineLimits{})
	got := make([]string, len(msgs))
	for i, m := range msgs {
		got[i] = m.String()
	}
	want := []string{
		"BATCH +r1 draft/multiline #go",
		"@batch=r1 PRIVMSG #go a",
		"@batch=r1 PRIVMSG #go b",
		"@batch=r1 PRIVMSG #go c",
		"BATCH -r1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("batch wire:\n got %q\nwant %q", got, want)
	}

	// max-lines truncates.
	capped := buildMultilineBatch("r2", "#go", []string{"1", "2", "3", "4"}, multilineLimits{maxLines: 2})
	if len(capped) != 4 { // open + 2 lines + close
		t.Fatalf("max-lines not honored: %d messages", len(capped))
	}
}

func TestParseMultilineLimits(t *testing.T) {
	lim := parseMultilineLimits("max-bytes=4096,max-lines=24")
	if lim.maxBytes != 4096 || lim.maxLines != 24 {
		t.Fatalf("limits = %+v", lim)
	}
	if got := parseMultilineLimits(""); got.maxBytes != 0 || got.maxLines != 0 {
		t.Fatalf("empty = %+v", got)
	}
	if got := parseMultilineLimits("max-lines=10"); got.maxLines != 10 || got.maxBytes != 0 {
		t.Fatalf("partial = %+v", got)
	}
}
