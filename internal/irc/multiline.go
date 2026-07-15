package irc

import (
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

// mlAccum accumulates one open multiline batch.
type mlAccum struct {
	target  string
	command string        // PRIVMSG or NOTICE, from the first line
	prefix  *ircv4.Prefix // message author
	tags    ircv4.Tags    // msgid/time for the whole message (from BATCH open)
	body    strings.Builder
	first   bool
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
// is non-nil only on batch close, carrying the reconstructed message.
func (ml *multiline) feed(in *ircv4.Message) (emit *ircv4.Message, consumed bool) {
	// Batch open: "BATCH +<ref> draft/multiline <target>".
	if in.Command == "BATCH" && len(in.Params) >= 3 &&
		strings.HasPrefix(in.Params[0], "+") && in.Params[1] == "draft/multiline" {
		ml.batches[in.Params[0][1:]] = &mlAccum{
			target: in.Params[2],
			prefix: in.Prefix,
			tags:   in.Tags.Copy(),
			first:  true,
		}
		return nil, true
	}
	// Batch close: reconstruct if it was a multiline batch we tracked.
	if in.Command == "BATCH" && len(in.Params) >= 1 && strings.HasPrefix(in.Params[0], "-") {
		ref := in.Params[0][1:]
		acc, ok := ml.batches[ref]
		if !ok {
			return nil, false // some other batch type; let the caller handle it
		}
		delete(ml.batches, ref)
		return acc.build(), true
	}
	// A line inside a tracked multiline batch.
	if ref := in.Tags["batch"]; ref != "" {
		if acc, ok := ml.batches[ref]; ok {
			acc.add(in)
			return nil, true
		}
	}
	return nil, false
}

// add appends one PRIVMSG/NOTICE line to the accumulated message.
func (a *mlAccum) add(in *ircv4.Message) {
	if in.Command != "PRIVMSG" && in.Command != "NOTICE" {
		return // non-message lines in a multiline batch are ignored
	}
	if a.first {
		a.command = in.Command
		if a.prefix == nil {
			a.prefix = in.Prefix
		}
		a.first = false
	} else if _, concat := in.Tags[concatTag]; !concat {
		a.body.WriteByte('\n')
	}
	a.body.WriteString(in.Trailing())
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

// buildMultilineBatch turns a multi-line message into the wire messages of
// a draft/multiline batch: BATCH open, one PRIVMSG per line, BATCH close.
// Lines beyond max-lines are dropped (the composer should prevent this);
// max-bytes is not split with concat here — long single lines are sent
// as-is and the server may reject with FAIL, which we surface.
func buildMultilineBatch(ref, target string, lines []string, lim multilineLimits) []*ircv4.Message {
	if lim.maxLines > 0 && len(lines) > lim.maxLines {
		lines = lines[:lim.maxLines]
	}
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
