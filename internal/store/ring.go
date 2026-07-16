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
}

func newRing(max int) *ring {
	return &ring{max: max}
}

func cursorLess(a, b Cursor) bool {
	return a.TS < b.TS || (a.TS == b.TS && a.ID < b.ID)
}

// insert places m in cursor order and evicts the oldest entry when over
// capacity. Live traffic is almost always an append at the end; the sorted
// insert only matters for slightly out-of-order server-time values.
func (r *ring) insert(m Message) {
	c := m.Cursor()
	i := sort.Search(len(r.msgs), func(i int) bool {
		return cursorLess(c, r.msgs[i].Cursor())
	})
	r.msgs = append(r.msgs, Message{})
	copy(r.msgs[i+1:], r.msgs[i:])
	r.msgs[i] = m
	if len(r.msgs) > r.max {
		// The evicted message (possibly m itself, if it predates
		// everything here) lives only on disk now.
		r.msgs = append(r.msgs[:0], r.msgs[1:]...)
		r.complete = false
	}
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
func (r *ring) adoptMsgID(id int64, msgid string) {
	for i := range r.msgs {
		if r.msgs[i].ID == id {
			r.msgs[i].MsgID = msgid
			return
		}
	}
}

func (r *ring) redact(msgid, reason string) {
	for i := range r.msgs {
		if r.msgs[i].MsgID == msgid {
			r.msgs[i].Redacted = true
			r.msgs[i].RedactReason = reason
			return
		}
	}
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
