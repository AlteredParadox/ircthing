package irc

import (
	"fmt"
	"strconv"
	"strings"

	ircv4 "gopkg.in/irc.v4"
)

// draft/multiline (https://ircv3.net/specs/extensions/multiline, fetched
// 2026-07-15): a PRIVMSG/NOTICE too large or with embedded newlines is
// sent as a `draft/multiline` BATCH of individual lines, joined by "\n"
// (or directly when a line carries the draft/multiline-concat tag). We
// reconstruct incoming batches into one logical message and split
// outgoing ones into a compliant batch.
//
// Nested batches (a multiline batch replayed inside a chathistory batch)
// are not handled specially — the reconstructed message is emitted as a
// normal event; marking it as replay would require batch nesting the read
// loop does not track. Servers offering both are rare and this is
// untested against a live server (neither Ergo nor Libera advertise it).

const concatTag = "draft/multiline-concat"

// Defensive ceilings on INCOMING multiline reconstruction. A batch's
// lines are consumed by the read loop (never reaching the bounded event
// channel) and held in memory until the server sends the matching close,
// so without these a malicious or compromised server could open batches
// without bound or stream indefinitely into one. These are independent
// of any draft/multiline limits we advertise for our own sends (those
// bound what WE send); exceeding any of them tears the connection down.
const (
	maxOpenMLBatches = 8
	maxMLBatchLines  = 1024
	maxMLBatchBytes  = 64 * 1024
)

// mlAccum accumulates one open multiline batch.
type mlAccum struct {
	target  string
	command string        // PRIVMSG or NOTICE, from the first line
	prefix  *ircv4.Prefix // message author
	tags    ircv4.Tags    // msgid/time for the whole message (from BATCH open)
	body    strings.Builder
	first   bool
	lines   int
}

// multiline reconstructs incoming multiline batches. It is used only by
// the per-connection read loop, so it needs no locking.
type multiline struct {
	batches map[string]*mlAccum // reference tag -> accumulator
}

func newMultiline() *multiline {
	return &multiline{batches: make(map[string]*mlAccum)}
}

// feed processes one incoming message. consumed is true when the message
// is part of a multiline batch (and must not be handled normally); emit
// is non-nil only on batch close, carrying the reconstructed message. A
// non-nil error means a defensive limit was exceeded and the caller must
// tear the connection down.
func (ml *multiline) feed(in *ircv4.Message) (emit *ircv4.Message, consumed bool, err error) {
	// Batch open: "BATCH +<ref> draft/multiline <target>".
	if in.Command == "BATCH" && len(in.Params) >= 3 &&
		strings.HasPrefix(in.Params[0], "+") && in.Params[1] == "draft/multiline" {
		if len(ml.batches) >= maxOpenMLBatches {
			return nil, true, fmt.Errorf("irc: too many open multiline batches (>%d)", maxOpenMLBatches)
		}
		ml.batches[in.Params[0][1:]] = &mlAccum{
			target: in.Params[2],
			prefix: in.Prefix,
			tags:   in.Tags.Copy(),
			first:  true,
		}
		return nil, true, nil
	}
	// Batch close: reconstruct if it was a multiline batch we tracked.
	if in.Command == "BATCH" && len(in.Params) >= 1 && strings.HasPrefix(in.Params[0], "-") {
		ref := in.Params[0][1:]
		acc, ok := ml.batches[ref]
		if !ok {
			return nil, false, nil // some other batch type; let the caller handle it
		}
		delete(ml.batches, ref)
		return acc.build(), true, nil
	}
	// A line inside a tracked multiline batch.
	if ref := in.Tags["batch"]; ref != "" {
		if acc, ok := ml.batches[ref]; ok {
			if err := acc.add(in); err != nil {
				return nil, true, err
			}
			return nil, true, nil
		}
	}
	return nil, false, nil
}

// add appends one PRIVMSG/NOTICE line to the accumulated message, or
// errors when the batch would exceed a defensive line/byte ceiling.
func (a *mlAccum) add(in *ircv4.Message) error {
	if in.Command != "PRIVMSG" && in.Command != "NOTICE" {
		return nil // non-message lines in a multiline batch are ignored
	}
	body := in.Trailing()
	newline := !a.first
	if _, concat := in.Tags[concatTag]; concat {
		newline = false
	}
	sep := 0
	if newline {
		sep = 1
	}
	if a.lines+1 > maxMLBatchLines {
		return fmt.Errorf("irc: multiline batch exceeded %d lines", maxMLBatchLines)
	}
	if a.body.Len()+sep+len(body) > maxMLBatchBytes {
		return fmt.Errorf("irc: multiline batch exceeded %d bytes", maxMLBatchBytes)
	}
	if a.first {
		a.command = in.Command
		if a.prefix == nil {
			a.prefix = in.Prefix
		}
		a.first = false
	} else if newline {
		a.body.WriteByte('\n')
	}
	a.body.WriteString(body)
	a.lines++
	return nil
}

// build synthesizes the single reconstructed message, or nil if the batch
// carried no message lines.
func (a *mlAccum) build() *ircv4.Message {
	if a.command == "" {
		return nil
	}
	return &ircv4.Message{
		Tags:    a.tags,
		Prefix:  a.prefix,
		Command: a.command,
		Params:  []string{a.target, a.body.String()},
	}
}

// multilineLimits holds the max-bytes/max-lines from the cap value.
type multilineLimits struct {
	maxBytes int
	maxLines int
}

// parseMultilineLimits reads the "max-bytes=..,max-lines=.." cap value.
// Zero means unspecified/no limit for that field.
func parseMultilineLimits(value string) multilineLimits {
	var lim multilineLimits
	for _, tok := range strings.Split(value, ",") {
		k, v, _ := strings.Cut(tok, "=")
		n, _ := strconv.Atoi(v)
		switch k {
		case "max-bytes":
			lim.maxBytes = n
		case "max-lines":
			lim.maxLines = n
		}
	}
	return lim
}

// validateMultiline checks a prospective outgoing batch against the
// server's advertised limits BEFORE anything is enqueued: max-lines and
// max-bytes from the draft/multiline cap value, and each line against
// the line length (ISUPPORT LINELEN, default 512) minus the batch
// framing overhead. Nothing is ever silently truncated — an oversized
// message is rejected whole, with an error naming the limit.
func validateMultiline(target string, lines []string, lim multilineLimits, lineLen int) error {
	if lim.maxLines > 0 && len(lines) > lim.maxLines {
		return fmt.Errorf("message is %d lines; the server allows at most %d per message", len(lines), lim.maxLines)
	}
	if lineLen <= 0 {
		lineLen = 512
	}
	// Worst-case per-line framing: "@batch=<ref> PRIVMSG <target> :" +
	// CRLF, with the ref up to ~22 bytes ("ml" + uint64).
	overhead := len("@batch=ml18446744073709551615 PRIVMSG ") + len(target) + len(" :\r\n")
	total := 0
	for _, line := range lines {
		if len(line)+overhead > lineLen {
			return fmt.Errorf("a line is %d bytes; the server's %d-byte line limit allows %d here", len(line), lineLen, lineLen-overhead)
		}
		total += len(line)
	}
	total += len(lines) - 1 // the newlines the joined message represents
	if lim.maxBytes > 0 && total > lim.maxBytes {
		return fmt.Errorf("message is %d bytes; the server allows at most %d per multiline message", total, lim.maxBytes)
	}
	return nil
}

// buildMultilineBatch turns a multi-line message into the wire messages of
// a draft/multiline batch: BATCH open, one PRIVMSG per line, BATCH close.
// The lines must already have passed validateMultiline.
func buildMultilineBatch(ref, target string, lines []string) []*ircv4.Message {
	out := make([]*ircv4.Message, 0, len(lines)+2)
	out = append(out, &ircv4.Message{Command: "BATCH", Params: []string{"+" + ref, "draft/multiline", target}})
	for _, line := range lines {
		out = append(out, &ircv4.Message{
			Tags:    ircv4.Tags{"batch": ref},
			Command: "PRIVMSG",
			Params:  []string{target, line},
		})
	}
	out = append(out, &ircv4.Message{Command: "BATCH", Params: []string{"-" + ref}})
	return out
}
