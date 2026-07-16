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
// kept as display characters, all of them ordered highest first when
// multi-prefix (https://ircv3.net/specs/extensions/multi-prefix, fetched
// 2026-07-15) is negotiated — without it only the highest is known.
// NAMES entries may be full nick!user@host hostmasks under
// userhost-in-names (spec fetched 2026-07-15).
//
// Away state and accounts come from three sources: the WHOX query the
// manager issues after each channel's NAMES (354 replies; see
// maybeWHOX), extended-join for members joining after us, and the
// away-notify / account-notify change streams (specs fetched
// 2026-07-16).
//
// Prefix ranks, MODE argument consumption and name casemapping all come
// from the connection's RPL_ISUPPORT values (PREFIX, CHANMODES,
// CASEMAPPING), falling back to the RFC 1459 defaults until 005 arrives.

// Member is one channel occupant.
type Member struct {
	Nick    string
	Prefix  string // status prefixes, highest first: e.g. "@+", "" for none
	Away    bool
	Account string // services account, "" when logged out or unknown
}

type channelState struct {
	name    string // original casing
	topic   string
	members map[string]Member // folded nick -> member
	pending map[string]Member // NAMES accumulation until 366
}

// roster is written by the connection's read loop and snapshotted by hub
// sessions, so a mutex guards the maps.
type roster struct {
	isup *isupport

	mu    sync.Mutex
	chans map[string]*channelState // folded channel -> state
}

func newRoster(isup *isupport) *roster {
	return &roster{isup: isup, chans: make(map[string]*channelState)}
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
	st := r.chans[r.isup.Fold(name)]
	if st == nil {
		return "", nil, false
	}
	members = make([]Member, 0, len(st.members))
	for _, m := range st.members {
		members = append(members, m)
	}
	sort.Slice(members, func(i, j int) bool {
		return r.isup.Fold(members[i].Nick) < r.isup.Fold(members[j].Nick)
	})
	return st.topic, members, true
}

// handle updates state from one server message. ourNick identifies which
// JOIN/PART/KICK are about us.
func (r *roster) handle(ourNick string, m *ircv4.Message) {
	r.mu.Lock()
	defer r.mu.Unlock()

	fold := r.isup.Fold
	sender := ""
	if m.Prefix != nil {
		sender = m.Prefix.Name
	}
	us := func(nick string) bool { return ourNick != "" && r.isup.FoldEqual(nick, ourNick) }

	switch m.Command {
	case "353": // RPL_NAMREPLY: <me> <symbol> <channel> :<prefixed nicks>
		st := r.chans[fold(m.Param(2))]
		if st == nil {
			return
		}
		if st.pending == nil {
			st.pending = make(map[string]Member)
		}
		for _, raw := range strings.Fields(m.Param(3)) {
			prefix, nick := splitNamesPrefix(r.isup.PrefixSymbols(), raw)
			st.pending[fold(nick)] = Member{Nick: nick, Prefix: prefix}
		}

	case "366": // RPL_ENDOFNAMES: <me> <channel> — pending replaces live
		if st := r.chans[fold(m.Param(1))]; st != nil && st.pending != nil {
			// NAMES carries no away/account data; keep what WHOX and the
			// notify streams already taught us about surviving members.
			for k, mem := range st.pending {
				if old, ok := st.members[k]; ok {
					mem.Away, mem.Account = old.Away, old.Account
					st.pending[k] = mem
				}
			}
			st.members = st.pending
			st.pending = nil
		}

	case "332": // RPL_TOPIC: <me> <channel> :<topic>
		if st := r.chans[fold(m.Param(1))]; st != nil {
			st.topic = m.Param(2)
		}

	case "331": // RPL_NOTOPIC
		if st := r.chans[fold(m.Param(1))]; st != nil {
			st.topic = ""
		}

	case "TOPIC":
		if st := r.chans[fold(m.Param(0))]; st != nil {
			st.topic = m.Trailing()
		}

	case "JOIN":
		ch := m.Param(0)
		if us(sender) {
			r.chans[fold(ch)] = &channelState{name: ch, members: make(map[string]Member)}
		}
		if st := r.chans[fold(ch)]; st != nil {
			mem := Member{Nick: sender}
			// extended-join (spec fetched 2026-07-16): JOIN carries
			// <account> <realname>, account "*" when logged out.
			if acct := m.Param(1); len(m.Params) >= 3 && acct != "*" {
				mem.Account = acct
			}
			st.members[fold(sender)] = mem
		}

	case "PART":
		if us(sender) {
			delete(r.chans, fold(m.Param(0)))
		} else if st := r.chans[fold(m.Param(0))]; st != nil {
			delete(st.members, fold(sender))
		}

	case "KICK": // <channel> <victim>
		victim := m.Param(1)
		if us(victim) {
			delete(r.chans, fold(m.Param(0)))
		} else if st := r.chans[fold(m.Param(0))]; st != nil {
			delete(st.members, fold(victim))
		}

	case "QUIT":
		for _, st := range r.chans {
			delete(st.members, fold(sender))
		}

	case "NICK":
		to := m.Param(0)
		for _, st := range r.chans {
			if mem, ok := st.members[fold(sender)]; ok {
				delete(st.members, fold(sender))
				mem.Nick = to
				st.members[fold(to)] = mem
			}
		}

	case "AWAY": // away-notify: a parameter means away, none means back
		away := len(m.Params) > 0
		for _, st := range r.chans {
			if mem, ok := st.members[fold(sender)]; ok {
				mem.Away = away
				st.members[fold(sender)] = mem
			}
		}

	case "ACCOUNT": // account-notify: <account>, "*" means logged out
		acct := m.Param(0)
		if acct == "*" {
			acct = ""
		}
		for _, st := range r.chans {
			if mem, ok := st.members[fold(sender)]; ok {
				mem.Account = acct
				st.members[fold(sender)] = mem
			}
		}

	case "354": // RPL_WHOSPCRPL: our WHOX reply — <me> <token> <nick> <flags> <account>
		if m.Param(1) != whoxToken || len(m.Params) < 5 {
			return
		}
		nick, flags, acct := m.Param(2), m.Param(3), m.Param(4)
		if acct == "0" { // logged out, per the WHOX spec
			acct = ""
		}
		away := strings.ContainsRune(flags, 'G')
		// The reply names no channel (we don't request the c field);
		// away/account are nick-level facts, applied wherever the nick is.
		for _, st := range r.chans {
			if mem, ok := st.members[fold(nick)]; ok {
				mem.Away = away
				mem.Account = acct
				st.members[fold(nick)] = mem
			}
		}

	case "MODE":
		if st := r.chans[fold(m.Param(0))]; st != nil {
			r.applyChannelMode(st, m.Params)
		}
	}
}

// splitNamesPrefix splits a NAMES entry like "@+nick" or, under
// userhost-in-names, "@+nick!user@host" into its status prefixes and the
// bare nick. multi-prefix sends all prefixes "in order of 'rank', from
// highest to lowest" — keep them as-is.
func splitNamesPrefix(symbols, raw string) (prefixes, nick string) {
	i := 0
	for i < len(raw) && strings.IndexByte(symbols, raw[i]) != -1 {
		i++
	}
	prefixes, nick = raw[:i], raw[i:]
	if bang := strings.IndexByte(nick, '!'); bang != -1 {
		nick = nick[:bang]
	}
	return prefixes, nick
}

// applyChannelMode parses a channel MODE change (params: channel,
// modestring, args...) and updates member status prefixes. Argument
// consumption follows the ISUPPORT CHANMODES classification; status
// (PREFIX) modes always take a nick argument. Unknown mode letters are
// assumed argument-less. Caller holds r.mu.
func (r *roster) applyChannelMode(st *channelState, params []string) {
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
		default:
			switch r.isup.ChanModeType(c) {
			case 'P': // status mode
				nick := takeArg()
				mem, ok := st.members[r.isup.Fold(nick)]
				if !ok {
					continue
				}
				sym := r.isup.SymbolForMode(c)
				if adding {
					mem.Prefix = addPrefix(r.isup.PrefixSymbols(), mem.Prefix, sym)
				} else {
					mem.Prefix = strings.ReplaceAll(mem.Prefix, sym, "")
				}
				st.members[r.isup.Fold(nick)] = mem
			case 'A', 'B': // always take an argument
				takeArg()
			case 'C': // argument only when setting
				if adding {
					takeArg()
				}
				// 'D' and unknown: no argument
			}
		}
	}
}

// addPrefix inserts a status prefix keeping rank order (highest first,
// per the ISUPPORT PREFIX ordering). With multi-prefix negotiated the
// stored set is exact; without it NAMES only reveals the highest, so
// lower statuses may be missing until a MODE grants them again.
func addPrefix(rank, current, changed string) string {
	if changed == "" || strings.Contains(current, changed) {
		return current
	}
	pos := strings.Index(rank, changed)
	for i := 0; i < len(current); i++ {
		if strings.IndexByte(rank, current[i]) > pos {
			return current[:i] + changed + current[i:]
		}
	}
	return current + changed
}
