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
	Bot     bool   // bot mode set (WHOX flags contain the ISUPPORT BOT letter)
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

// channelsWith returns the (sorted) channels nick is currently in.
func (r *roster) channelsWith(nick string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := r.isup.Fold(nick)
	var out []string
	for _, st := range r.chans {
		if _, ok := st.members[key]; ok {
			out = append(out, st.name)
		}
	}
	sort.Strings(out)
	return out
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
	case "353": // RPL_NAMREPLY
		r.namesReply(m)
	case "366": // RPL_ENDOFNAMES
		r.namesEnd(m)
	case "332": // RPL_TOPIC: <me> <channel> :<topic>
		r.setTopic(m.Param(1), m.Param(2))
	case "331": // RPL_NOTOPIC
		r.setTopic(m.Param(1), "")
	case "TOPIC":
		r.setTopic(m.Param(0), m.Trailing())
	case "JOIN":
		r.memberJoin(m, sender, us(sender))
	case "PART":
		r.memberLeft(m.Param(0), sender, us(sender))
	case "KICK": // <channel> <victim>
		r.memberLeft(m.Param(0), m.Param(1), us(m.Param(1)))
	case "QUIT":
		for _, st := range r.chans {
			delete(st.members, fold(sender))
		}
	case "NICK":
		r.rename(sender, m.Param(0))
	case "AWAY": // away-notify: a parameter means away, none means back
		away := len(m.Params) > 0
		r.updateEverywhere(sender, func(mem Member) Member {
			mem.Away = away
			return mem
		})
	case "ACCOUNT": // account-notify: <account>, "*" means logged out
		acct := m.Param(0)
		if acct == "*" {
			acct = ""
		}
		r.updateEverywhere(sender, func(mem Member) Member {
			mem.Account = acct
			return mem
		})
	case "354": // RPL_WHOSPCRPL: our WHOX reply
		r.whoxReply(m)
	case "MODE":
		if st := r.chans[fold(m.Param(0))]; st != nil {
			r.applyChannelMode(st, m.Params)
		}
	}
}

// setTopic updates a known channel's topic. Caller holds r.mu.
func (r *roster) setTopic(channel, topic string) {
	if st := r.chans[r.isup.Fold(channel)]; st != nil {
		st.topic = topic
	}
}

// memberLeft removes nick from channel (PART/KICK); when the departure
// is ours the whole channel state goes. Caller holds r.mu.
func (r *roster) memberLeft(channel, nick string, ours bool) {
	if ours {
		delete(r.chans, r.isup.Fold(channel))
		return
	}
	if st := r.chans[r.isup.Fold(channel)]; st != nil {
		delete(st.members, r.isup.Fold(nick))
	}
}

// namesReply accumulates one 353 line: <me> <symbol> <channel>
// :<prefixed nicks>. Caller holds r.mu.
func (r *roster) namesReply(m *ircv4.Message) {
	st := r.chans[r.isup.Fold(m.Param(2))]
	if st == nil {
		return
	}
	if st.pending == nil {
		st.pending = make(map[string]Member)
	}
	for _, raw := range strings.Fields(m.Param(3)) {
		prefix, nick := splitNamesPrefix(r.isup.PrefixSymbols(), raw)
		st.pending[r.isup.Fold(nick)] = Member{Nick: nick, Prefix: prefix}
	}
}

// namesEnd swaps the accumulated NAMES in on 366: <me> <channel>. NAMES
// carries no away/account/bot data, so what WHOX and the notify streams
// already taught us about surviving members is kept. Caller holds r.mu.
func (r *roster) namesEnd(m *ircv4.Message) {
	st := r.chans[r.isup.Fold(m.Param(1))]
	if st == nil || st.pending == nil {
		return
	}
	for k, mem := range st.pending {
		if old, ok := st.members[k]; ok {
			mem.Away, mem.Account, mem.Bot = old.Away, old.Account, old.Bot
			st.pending[k] = mem
		}
	}
	st.members = st.pending
	st.pending = nil
}

// memberJoin records a JOIN; ours creates the channel. extended-join
// (spec fetched 2026-07-16) carries <account> <realname>, account "*"
// when logged out. Caller holds r.mu.
func (r *roster) memberJoin(m *ircv4.Message, sender string, ours bool) {
	ch := m.Param(0)
	if ours {
		r.chans[r.isup.Fold(ch)] = &channelState{name: ch, members: make(map[string]Member)}
	}
	st := r.chans[r.isup.Fold(ch)]
	if st == nil {
		return
	}
	mem := Member{Nick: sender}
	if acct := m.Param(1); len(m.Params) >= 3 && acct != "*" {
		mem.Account = acct
	}
	st.members[r.isup.Fold(sender)] = mem
}

// rename re-keys a nick in every channel, keeping its state. Caller
// holds r.mu.
func (r *roster) rename(from, to string) {
	fold := r.isup.Fold
	for _, st := range r.chans {
		if mem, ok := st.members[fold(from)]; ok {
			delete(st.members, fold(from))
			mem.Nick = to
			st.members[fold(to)] = mem
		}
	}
}

// updateEverywhere applies fn to nick's membership in every channel —
// the shape of nick-level facts (AWAY, ACCOUNT, WHOX). Caller holds r.mu.
func (r *roster) updateEverywhere(nick string, fn func(Member) Member) {
	key := r.isup.Fold(nick)
	for _, st := range r.chans {
		if mem, ok := st.members[key]; ok {
			st.members[key] = fn(mem)
		}
	}
}

// whoxReply applies one of our WHOX 354 replies: <me> <token> <nick>
// <flags> <account>. The reply names no channel (we don't request the c
// field); away/account/bot are nick-level facts, applied wherever the
// nick is. Caller holds r.mu.
func (r *roster) whoxReply(m *ircv4.Message) {
	if m.Param(1) != whoxToken || len(m.Params) < 5 {
		return
	}
	nick, flags, acct := m.Param(2), m.Param(3), m.Param(4)
	if acct == "0" { // logged out, per the WHOX spec
		acct = ""
	}
	away := strings.ContainsRune(flags, 'G')
	// Bot mode (https://ircv3.net/specs/extensions/bot-mode, fetched
	// 2026-07-16): the ISUPPORT BOT letter appears in WHO flags.
	bot := false
	if letter, ok := r.isup.Raw("BOT"); ok && letter != "" {
		bot = strings.Contains(flags, letter)
	}
	r.updateEverywhere(nick, func(mem Member) Member {
		mem.Away = away
		mem.Account = acct
		mem.Bot = bot
		return mem
	})
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
				r.applyStatusMode(st, takeArg(), r.isup.SymbolForMode(c), adding)
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

// applyStatusMode grants or revokes one status prefix on a member.
// Caller holds r.mu.
func (r *roster) applyStatusMode(st *channelState, nick, sym string, adding bool) {
	mem, ok := st.members[r.isup.Fold(nick)]
	if !ok {
		return
	}
	if adding {
		mem.Prefix = addPrefix(r.isup.PrefixSymbols(), mem.Prefix, sym)
	} else {
		mem.Prefix = strings.ReplaceAll(mem.Prefix, sym, "")
	}
	st.members[r.isup.Fold(nick)] = mem
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
