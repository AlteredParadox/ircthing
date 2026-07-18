package store

import "sort"

// ring is the bounded in-memory hot scrollback for one buffer: the newest
// `cap` messages, kept sorted by cursor (ts, id) so pages served from it
// are ordered identically to the SQL ORDER BY.
//
// The serving rule (see Store.Before/After for the other half):
//
//   - complete == true means the ring holds the buffer's ENTIRE history
//     (nothing has ever been evicted or left unwarmed), so any page can be
//     answered from memory.
//   - Otherwise a Before page is served from memory only when the ring
//     alone fills it (len == limit) — a partial fill might be missing
//     older rows that exist on disk.
//   - An After page is served from memory when the cursor is at or past
//     the ring's oldest entry: the ring is the newest suffix of history,
//     so everything after such a cursor is here.
type ring struct {
	max      int
	msgs     []Message // ascending by (ts, id)
	complete bool
	// bytes is the running sum of msgBytes over msgs — the ring's estimated
	// resident cost, kept in step by every mutation so the Store can enforce a
	// global hot-ring byte budget without rescanning. lastUsed is the LRU access
	// sequence, stamped by Store.touchRing.
	bytes    int
	lastUsed uint64
}

func newRing(max int) *ring {
	return &ring{max: max}
}

// msgOverhead approximates a Message's fixed retained cost beyond its string
// CONTENT: the string headers, Time/ID/bool fields, and per-element slice slack.
// The budget only needs to be roughly right — it bounds memory, not bills it.
const msgOverhead = 128

// msgBytes estimates the bytes a Message keeps resident in a ring: all its
// (server-controlled, already-clamped) string content plus the fixed overhead.
func msgBytes(m Message) int {
	return len(m.Network) + len(m.Target) + len(m.Sender) + len(m.MsgID) +
		len(m.Command) + len(m.Raw) + len(m.Text) + len(m.RedactReason) + msgOverhead
}

func cursorLess(a, b Cursor) bool {
	return a.TS < b.TS || (a.TS == b.TS && a.ID < b.ID)
}

// insert places m in cursor order and evicts the oldest entry when over
// capacity. Live traffic is almost always an append at the end; the sorted
// insert only matters for slightly out-of-order server-time values. Returns the
// change in r.bytes so the Store can track the global total.
func (r *ring) insert(m Message) int {
	before := r.bytes
	c := m.Cursor()
	i := sort.Search(len(r.msgs), func(i int) bool {
		return cursorLess(c, r.msgs[i].Cursor())
	})
	r.msgs = append(r.msgs, Message{})
	copy(r.msgs[i+1:], r.msgs[i:])
	r.msgs[i] = m
	r.bytes += msgBytes(m)
	if len(r.msgs) > r.max {
		// The evicted message (possibly m itself, if it predates
		// everything here) lives only on disk now.
		r.bytes -= msgBytes(r.msgs[0])
		r.dropFirst(1)
		r.complete = false
	}
	return r.bytes - before
}

// dropFirst removes the oldest n entries, compacting in place and clearing
// the vacated tail slots: without the clear, the backing array would keep
// the dropped messages' strings GC-reachable while bytes reports them
// freed — a pruned-then-quiet buffer would retain its entire dropped
// prefix indefinitely.
func (r *ring) dropFirst(n int) {
	kept := copy(r.msgs, r.msgs[n:])
	clear(r.msgs[kept:])
	r.msgs = r.msgs[:kept]
}

// trimToBytes drops oldest entries until the ring's accounted bytes are at
// most limit, always keeping the newest message. Returns the (non-positive)
// byte delta. This exists because evictRings can never evict the in-use
// ring: with a large configured ring_size and near-clamp-size messages a
// single buffer could otherwise outgrow the entire global budget.
func (r *ring) trimToBytes(limit int) int {
	before := r.bytes
	n := 0
	for n < len(r.msgs)-1 && r.bytes > limit {
		r.bytes -= msgBytes(r.msgs[n])
		n++
	}
	if n > 0 {
		r.dropFirst(n)
		// The trimmed prefix still exists on disk (unlike applyRetention,
		// this mirrors no delete), so the ring is no longer all of history.
		r.complete = false
	}
	return r.bytes - before
}

// pageBefore returns up to limit messages with cursor < c (ascending) and
// whether the ring alone could answer authoritatively.
func (r *ring) pageBefore(c Cursor, limit int) ([]Message, bool) {
	i := sort.Search(len(r.msgs), func(i int) bool {
		return !cursorLess(r.msgs[i].Cursor(), c)
	})
	lo := i - limit
	if lo < 0 {
		lo = 0
	}
	out := clone(r.msgs[lo:i])
	return out, r.complete || len(out) == limit
}

// pageAfter returns up to limit messages with cursor > c (ascending) and
// whether the ring alone could answer authoritatively.
func (r *ring) pageAfter(c Cursor, limit int) ([]Message, bool) {
	i := sort.Search(len(r.msgs), func(i int) bool {
		return cursorLess(c, r.msgs[i].Cursor())
	})
	hi := i + limit
	if hi > len(r.msgs) {
		hi = len(r.msgs)
	}
	out := clone(r.msgs[i:hi])
	ok := r.complete ||
		(len(r.msgs) > 0 && !cursorLess(c, r.msgs[0].Cursor()))
	return out, ok
}

// redact marks a cached message (by msgid) as deleted so ring-served
// history pages reflect the redaction without a database round-trip.
// adoptMsgID stamps a msgid onto the ring's copy of a row (matched by
// id), keeping the hot cache consistent with AdoptOwnMsgID.
func (r *ring) adoptMsgID(id int64, msgid string) int {
	for i := range r.msgs {
		if r.msgs[i].ID == id {
			delta := len(msgid) - len(r.msgs[i].MsgID)
			r.msgs[i].MsgID = msgid
			r.bytes += delta
			return delta
		}
	}
	return 0
}

// applyRetention removes messages the store's retention pruning just
// deleted from disk, keeping the hot ring consistent without dropping (and
// having to re-warm) it. cutoffMs > 0 drops entries older than the cutoff
// (ts < cutoffMs, matching the DELETE); maxPerBuffer > 0 keeps only the
// newest N.
//
// complete is left unchanged, which is always safe: filtering a complete
// ring in step with the identical disk delete keeps it a faithful complete
// view, and leaving a non-complete ring non-complete only means reads may
// still fall back to disk (which now returns the same rows).
func (r *ring) applyRetention(cutoffMs int64, maxPerBuffer int) int {
	before := r.bytes
	if cutoffMs > 0 {
		// msgs are ascending by (ts, id): find the first kept entry.
		i := sort.Search(len(r.msgs), func(i int) bool {
			return r.msgs[i].Time.UnixMilli() >= cutoffMs
		})
		if i > 0 {
			for j := 0; j < i; j++ {
				r.bytes -= msgBytes(r.msgs[j])
			}
			r.dropFirst(i)
		}
	}
	if maxPerBuffer > 0 && len(r.msgs) > maxPerBuffer {
		drop := len(r.msgs) - maxPerBuffer
		for j := 0; j < drop; j++ {
			r.bytes -= msgBytes(r.msgs[j])
		}
		r.dropFirst(drop)
	}
	return r.bytes - before
}

func (r *ring) redact(msgid, reason string) int {
	for i := range r.msgs {
		if r.msgs[i].MsgID == msgid {
			old := len(r.msgs[i].Raw) + len(r.msgs[i].Text) + len(r.msgs[i].RedactReason)
			r.msgs[i].Redacted = true
			r.msgs[i].RedactReason = reason
			// Scrub the hot copy in step with the destructive DB update, so
			// pages served from memory do not leak the redacted body.
			r.msgs[i].Raw = ""
			r.msgs[i].Text = ""
			delta := len(reason) - old
			r.bytes += delta
			return delta
		}
	}
	return 0
}

// clone copies a page out of the ring so callers never alias its backing
// array (which insert mutates).
func clone(msgs []Message) []Message {
	if len(msgs) == 0 {
		return nil
	}
	out := make([]Message, len(msgs))
	copy(out, msgs)
	return out
}
