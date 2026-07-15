package irc

import (
	"sort"
	"strings"
	"sync"

	ircv4 "gopkg.in/irc.v4"
)

// Channel membership and topic tracking for one connection.
//
// Sources: NAMES on join (353 accumulated until 366), then live
// JOIN/PART/KICK/QUIT/NICK/MODE/TOPIC/332/331/AWAY. Status prefixes are
// kept as display characters ("~&@%+"), all of them ordered highest
// first when multi-prefix (https://ircv3.net/specs/extensions/
// multi-prefix, fetched 2026-07-15) is negotiated — without it only the
// highest is known. NAMES entries may be full nick!user@host hostmasks
// under userhost-in-names (spec fetched 2026-07-15). Away state comes
// from away-notify AWAY lines (spec fetched 2026-07-15). Casemapping is
// ASCII lowering and MODE parsing assumes the RFC 1459 defaults
// (PREFIX=(qaohv)~&@%+ superset, CHANMODES=b,k,l,imnpst) until ISUPPORT
// parsing lands.

// Member is one channel occupant.
type Member struct {
	Nick   string
	Prefix string // status prefixes, highest first: e.g. "@+", "" for none
	Away   bool   // known only when away-notify is negotiated
}

type channelState struct {
	name    string // original casing
	topic   string
	members map[string]Member // lower(nick) -> member
	pending map[string]Member // NAMES accumulation until 366
}

// roster is written by the connection's read loop and snapshotted by hub
// sessions, so a mutex guards the maps.
type roster struct {
	mu    sync.Mutex
	chans map[string]*channelState // lower(channel) -> state
}

func newRoster() *roster {
	return &roster{chans: make(map[string]*channelState)}
}

// clear drops all state; the manager calls it when a connection ends,
// since membership is only valid per connection.
func (r *roster) clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.chans = make(map[string]*channelState)
}

// channel returns the topic and a nick-sorted member snapshot.
func (r *roster) channel(name string) (topic string, members []Member, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.chans[lower(name)]
	if st == nil {
		return "", nil, false
	}
	members = make([]Member, 0, len(st.members))
	for _, m := range st.members {
		members = append(members, m)
	}
	sort.Slice(members, func(i, j int) bool {
		return lower(members[i].Nick) < lower(members[j].Nick)
	})
	return st.topic, members, true
}

// handle updates state from one server message. ourNick identifies which
// JOIN/PART/KICK are about us.
func (r *roster) handle(ourNick string, m *ircv4.Message) {
	r.mu.Lock()
	defer r.mu.Unlock()

	sender := ""
	if m.Prefix != nil {
		sender = m.Prefix.Name
	}
	us := func(nick string) bool { return lower(nick) == lower(ourNick) && ourNick != "" }

	switch m.Command {
	case "353": // RPL_NAMREPLY: <me> <symbol> <channel> :<prefixed nicks>
		st := r.chans[lower(m.Param(2))]
		if st == nil {
			return
		}
		if st.pending == nil {
			st.pending = make(map[string]Member)
		}
		for _, raw := range strings.Fields(m.Param(3)) {
			prefix, nick := splitNamesPrefix(raw)
			st.pending[lower(nick)] = Member{Nick: nick, Prefix: prefix}
		}

	case "366": // RPL_ENDOFNAMES: <me> <channel> — pending replaces live
		if st := r.chans[lower(m.Param(1))]; st != nil && st.pending != nil {
			st.members = st.pending
			st.pending = nil
		}

	case "332": // RPL_TOPIC: <me> <channel> :<topic>
		if st := r.chans[lower(m.Param(1))]; st != nil {
			st.topic = m.Param(2)
		}

	case "331": // RPL_NOTOPIC
		if st := r.chans[lower(m.Param(1))]; st != nil {
			st.topic = ""
		}

	case "TOPIC":
		if st := r.chans[lower(m.Param(0))]; st != nil {
			st.topic = m.Trailing()
		}

	case "JOIN":
		ch := m.Param(0)
		if us(sender) {
			r.chans[lower(ch)] = &channelState{name: ch, members: make(map[string]Member)}
		}
		if st := r.chans[lower(ch)]; st != nil {
			st.members[lower(sender)] = Member{Nick: sender}
		}

	case "PART":
		if us(sender) {
			delete(r.chans, lower(m.Param(0)))
		} else if st := r.chans[lower(m.Param(0))]; st != nil {
			delete(st.members, lower(sender))
		}

	case "KICK": // <channel> <victim>
		victim := m.Param(1)
		if us(victim) {
			delete(r.chans, lower(m.Param(0)))
		} else if st := r.chans[lower(m.Param(0))]; st != nil {
			delete(st.members, lower(victim))
		}

	case "QUIT":
		for _, st := range r.chans {
			delete(st.members, lower(sender))
		}

	case "NICK":
		to := m.Param(0)
		for _, st := range r.chans {
			if mem, ok := st.members[lower(sender)]; ok {
				delete(st.members, lower(sender))
				mem.Nick = to
				st.members[lower(to)] = mem
			}
		}

	case "AWAY": // away-notify: a parameter means away, none means back
		away := len(m.Params) > 0
		for _, st := range r.chans {
			if mem, ok := st.members[lower(sender)]; ok {
				mem.Away = away
				st.members[lower(sender)] = mem
			}
		}

	case "MODE":
		if st := r.chans[lower(m.Param(0))]; st != nil {
			applyChannelMode(st, m.Params)
		}
	}
}

// splitNamesPrefix splits a NAMES entry like "@+nick" or, under
// userhost-in-names, "@+nick!user@host" into its status prefixes and the
// bare nick. multi-prefix sends all prefixes "in order of 'rank', from
// highest to lowest" — keep them as-is.
func splitNamesPrefix(raw string) (prefixes, nick string) {
	i := 0
	for i < len(raw) && strings.IndexByte(prefixRank, raw[i]) != -1 {
		i++
	}
	prefixes, nick = raw[:i], raw[i:]
	if bang := strings.IndexByte(nick, '!'); bang != -1 {
		nick = nick[:bang]
	}
	return prefixes, nick
}

// prefixForMode maps a PREFIX mode letter to its display char.
var prefixForMode = map[byte]string{'q': "~", 'a': "&", 'o': "@", 'h': "%", 'v': "+"}

// prefixRank orders status prefixes, highest first.
const prefixRank = "~&@%+"

// applyChannelMode parses a channel MODE change (params: channel,
// modestring, args...) and updates member status prefixes. Argument
// consumption follows the RFC 1459 default CHANMODES: type A ("b") and
// type B ("k") always take an argument, type C ("l") only when setting,
// type D never; status modes (qaohv) always do.
func applyChannelMode(st *channelState, params []string) {
	if len(params) < 2 {
		return
	}
	arg := 2
	takeArg := func() string {
		if arg < len(params) {
			a := params[arg]
			arg++
			return a
		}
		return ""
	}
	adding := true
	for i := 0; i < len(params[1]); i++ {
		c := params[1][i]
		switch c {
		case '+':
			adding = true
		case '-':
			adding = false
		case 'q', 'a', 'o', 'h', 'v':
			nick := takeArg()
			mem, ok := st.members[lower(nick)]
			if !ok {
				continue
			}
			if adding {
				mem.Prefix = addPrefix(mem.Prefix, prefixForMode[c])
			} else {
				mem.Prefix = strings.ReplaceAll(mem.Prefix, prefixForMode[c], "")
			}
			st.members[lower(nick)] = mem
		case 'b', 'k': // always take an argument
			takeArg()
		case 'l': // argument only when setting
			if adding {
				takeArg()
			}
			// type D (imnpst...) and unknown modes: no argument
		}
	}
}

// addPrefix inserts a status prefix keeping rank order (highest first).
// With multi-prefix negotiated the stored set is exact; without it NAMES
// only reveals the highest, so lower statuses may be missing until a
// MODE grants them again.
func addPrefix(current, changed string) string {
	if strings.Contains(current, changed) {
		return current
	}
	rank := strings.Index(prefixRank, changed)
	for i := 0; i < len(current); i++ {
		if strings.IndexByte(prefixRank, current[i]) > rank {
			return current[:i] + changed + current[i:]
		}
	}
	return current + changed
}

func lower(s string) string {
	return strings.ToLower(s)
}
