package irc

import (
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	ircv4 "gopkg.in/irc.v4"
)

// maxRosterField bounds a stored roster string (nick/user/host/account/topic).
// Real values are tiny; the cap stops a hostile server's oversized field from
// bloating the roster even after cloning detaches it from the parsed line.
const maxRosterField = 512

// foldKey is the canonical member/channel map-key form: clamp a
// server-supplied name to the roster field bound, THEN fold under the
// connection's casemapping. Every storage site keys by this form (via a
// clampRoster'd variable), so every LOOKUP must use it too — folding the raw
// name would never match a key stored from an oversized (>maxRosterField)
// name, leaving ghost members/channels that only reconnect clears.
func (r *roster) foldKey(name string) string {
	return r.isup.Fold(clampRoster(name))
}

// clampRoster bounds s to maxRosterField bytes (trimming a trailing partial
// rune) AND detaches it from the parsed IRC line via a fresh copy.
func clampRoster(s string) string {
	if len(s) > maxRosterField {
		s = s[:maxRosterField]
		for len(s) > 0 {
			if r, size := utf8.DecodeLastRuneInString(s); r != utf8.RuneError || size != 1 {
				break
			}
			s = s[:len(s)-1]
		}
	}
	return strings.Clone(s)
}

// Channel membership and topic tracking for one connection.
//
// These bound roster growth against a hostile server spoofing self-JOINs
// or flooding NAMES; legitimate use stays far below them. The per-channel
// caps alone don't bound aggregate memory (a server could fill many
// channels), so maxRosterMembers is a connection-wide count budget across
// every channel's members + pending. Counts alone still don't bound bytes:
// each stored field is independently clamped to maxRosterField, so 100k
// hostile members with maxed-out fields would retain ~300 MB — hence
// maxRosterBytes, the aggregate byte ceiling over members, pending NAMES,
// and topics. Legitimate heavy use (tens of thousands of members with
// real-sized fields, ~150 B each) stays a few MB.
// var (not const) so tests can exercise the bounds at small scale.
var (
	maxRosterChannels = 4096
	maxChannelMembers = 25_000
	maxRosterMembers  = 100_000
	maxRosterBytes    = 8 << 20
)

// memberOverhead approximates a Member's fixed retained cost beyond its
// string content (struct fields, map entry). Rough is fine — the budget
// bounds memory, it doesn't bill it (same idea as the store's msgOverhead).
const memberOverhead = 96

// memberBytes estimates the bytes one membership keeps resident: the folded
// map key plus every stored string field plus the fixed overhead.
func memberBytes(key string, m Member) int {
	return len(key) + len(m.Nick) + len(m.Prefix) + len(m.Account) +
		len(m.User) + len(m.Host) + memberOverhead
}

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
	// User/Host are the ident and hostname, when known: from userhost-in-names
	// (nick!user@host in 353), the JOIN prefix, and CHGHOST updates. Empty when
	// the server doesn't advertise them.
	User string
	Host string
}

type channelState struct {
	name    string // original casing
	topic   string
	members map[string]Member // folded nick -> member
	pending map[string]Member // NAMES accumulation until 366
	// bytes is the running memberBytes sum over members + pending, plus the
	// topic — this channel's share of the roster byte budget. Kept in step
	// by put/del/setTopic; recomputed wholesale on the 366 swap.
	bytes int
}

// put stores mem under key in mp (one of st's two maps), keeping st.bytes in
// step. Returns the byte delta. Caller holds r.mu.
func (st *channelState) put(mp map[string]Member, key string, mem Member) int {
	delta := memberBytes(key, mem)
	if old, ok := mp[key]; ok {
		delta -= memberBytes(key, old)
	}
	mp[key] = mem
	st.bytes += delta
	return delta
}

// del removes key from mp (one of st's two maps), keeping st.bytes in step.
// Safe on a nil map. Caller holds r.mu.
func (st *channelState) del(mp map[string]Member, key string) {
	if old, ok := mp[key]; ok {
		st.bytes -= memberBytes(key, old)
		delete(mp, key)
	}
}

// recomputeBytes rebuilds st.bytes from scratch — used after the 366 swap,
// where the old members map is dropped and pending values were rewritten in
// place. Caller holds r.mu.
func (st *channelState) recomputeBytes() {
	n := len(st.topic)
	for k, m := range st.members {
		n += memberBytes(k, m)
	}
	for k, m := range st.pending {
		n += memberBytes(k, m)
	}
	st.bytes = n
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
	key := r.foldKey(nick)
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
	st := r.chans[r.foldKey(name)]
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
		// PART takes a comma-list of channels (RFC 2812 §3.2.2). Mainstream
		// servers split these before relaying, but handle the multi form so a
		// server that doesn't leaves no ghost members.
		for _, ch := range strings.Split(m.Param(0), ",") {
			r.memberLeft(ch, sender, us(sender))
		}
	case "KICK": // <channel>{,<channel>} <user>{,<user>} (RFC 2812 §3.2.8)
		chans := strings.Split(m.Param(0), ",")
		victims := strings.Split(m.Param(1), ",")
		for i, victim := range victims {
			ch := chans[0] // 1 channel, N victims: all from that channel
			if len(chans) == len(victims) {
				ch = chans[i] // parallel channel[i]/victim[i] form
			}
			r.memberLeft(ch, victim, us(victim))
		}
	case "QUIT":
		// Clamp+fold ONCE, outside the loop: Fold allocates twice per call,
		// so folding a raw (up to ~64 KiB) spoofed sender per channel across
		// 4096 channels would be ~0.5 GiB of transient allocation for one
		// line. Clamping also matches how member keys were stored.
		qk := fold(clampRoster(sender))
		for _, st := range r.chans {
			st.del(st.members, qk)
			st.del(st.pending, qk)
		}
	case "NICK":
		r.rename(sender, m.Param(0))
	case "CHGHOST": // chghost: ":nick!user@host CHGHOST <newuser> <newhost>"
		user, host := clampRoster(m.Param(0)), clampRoster(m.Param(1))
		r.updateEverywhere(sender, func(mem Member) Member {
			mem.User, mem.Host = user, host
			return mem
		})
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
		acct = clampRoster(acct) // bound + detach the server-supplied account
		r.updateEverywhere(sender, func(mem Member) Member {
			mem.Account = acct
			return mem
		})
	case "354": // RPL_WHOSPCRPL: our WHOX reply
		r.whoxReply(m)
	case "MODE":
		if st := r.chans[r.foldKey(m.Param(0))]; st != nil {
			r.applyChannelMode(st, m.Params)
		}
	}
}

// setTopic updates a known channel's topic. Caller holds r.mu.
func (r *roster) setTopic(channel, topic string) {
	if st := r.chans[r.foldKey(channel)]; st != nil {
		t := clampRoster(topic) // bound + detach from the parsed line
		st.bytes += len(t) - len(st.topic)
		st.topic = t
	}
}

// memberLeft removes nick from channel (PART/KICK); when the departure
// is ours the whole channel state goes. Caller holds r.mu.
func (r *roster) memberLeft(channel, nick string, ours bool) {
	if ours {
		delete(r.chans, r.foldKey(channel))
		return
	}
	if st := r.chans[r.foldKey(channel)]; st != nil {
		fk := r.foldKey(nick)
		st.del(st.members, fk)
		st.del(st.pending, fk)
	}
}

// totalBytes is the connection-wide roster byte total (every channel's
// running sum). O(channels), computed on demand at the growth sites like
// totalMembers. Caller holds r.mu.
func (r *roster) totalBytes() int {
	n := 0
	for _, st := range r.chans {
		n += st.bytes
	}
	return n
}

// totalMembers is the connection-wide count of held members (every
// channel's members plus in-flight NAMES pending). Computed on demand at
// the two growth sites — O(channels), so no drift-prone counter to
// maintain. Caller holds r.mu.
func (r *roster) totalMembers() int {
	n := 0
	for _, st := range r.chans {
		n += len(st.members) + len(st.pending)
	}
	return n
}

// namesReply accumulates one 353 line: <me> <symbol> <channel>
// :<prefixed nicks>. Caller holds r.mu.
func (r *roster) namesReply(m *ircv4.Message) {
	st := r.chans[r.foldKey(m.Param(2))]
	if st == nil {
		return
	}
	if st.pending == nil {
		st.pending = make(map[string]Member)
	}
	budget := maxRosterMembers - r.totalMembers() // remaining aggregate room
	bytesLeft := maxRosterBytes - r.totalBytes()  // remaining aggregate bytes
	for _, raw := range strings.Fields(m.Param(3)) {
		if len(st.pending) >= maxChannelMembers || budget <= 0 || bytesLeft <= 0 {
			break // bound a NAMES flood: per-channel, count-wide, and byte-wide
		}
		prefix, nick, user, host := splitNamesPrefix(r.isup.PrefixSymbols(), raw)
		// Clamp BEFORE folding: Fold allocates a same-length key, so a hostile
		// ~64 KiB nick would otherwise pin a 64 KiB map key even though the Member
		// value is clamped — defeating the count-based member budget (mirrors the
		// channel-name clamp in memberJoin).
		nick = clampRoster(nick)
		fk := r.isup.Fold(nick)
		if _, exists := st.pending[fk]; !exists {
			budget-- // only a new nick consumes the aggregate budget
		}
		// Clone so the stored Member does not alias the parsed 353 line
		// (irc.v4 slices every field out of one per-line buffer, which a
		// short nick would otherwise pin alive in the roster).
		bytesLeft -= st.put(st.pending, fk, Member{
			Nick: nick, Prefix: clampRoster(prefix),
			User: clampRoster(user), Host: clampRoster(host),
		})
	}
}

// namesEnd swaps the accumulated NAMES in on 366: <me> <channel>. NAMES
// carries no away/account/bot data, so what WHOX and the notify streams
// already taught us about surviving members is kept. Caller holds r.mu.
func (r *roster) namesEnd(m *ircv4.Message) {
	st := r.chans[r.foldKey(m.Param(1))]
	if st == nil || st.pending == nil {
		return
	}
	for k, mem := range st.pending {
		if old, ok := st.members[k]; ok {
			mem.Away, mem.Account, mem.Bot = old.Away, old.Account, old.Bot
			// Keep a previously-learned user@host if this NAMES burst didn't
			// carry one (server without userhost-in-names).
			if mem.User == "" && mem.Host == "" {
				mem.User, mem.Host = old.User, old.Host
			}
			st.pending[k] = mem
		}
	}
	st.members = st.pending
	st.pending = nil
	// One O(members) recompute per completed burst: the swap dropped the
	// old members map and the merge rewrote pending values in place.
	st.recomputeBytes()
}

// joinChannel resolves the channel state a JOIN applies to. Only our own
// JOIN may create it — and only if unknown: a repeat self-JOIN (a
// buggy/hostile server, or a netsplit rejoin edge) must not wipe the
// accumulated members and topic. The set is bounded against a server
// spoofing distinct self-JOINs; nil means unknown (or at the cap). Caller
// holds r.mu.
func (r *roster) joinChannel(ch string, ours bool) *channelState {
	key := r.isup.Fold(ch)
	if st := r.chans[key]; st != nil {
		return st
	}
	if !ours || len(r.chans) >= maxRosterChannels {
		return nil
	}
	st := &channelState{name: ch, members: make(map[string]Member)}
	r.chans[key] = st
	return st
}

// joinMember builds the Member a JOIN line describes, every field clamped
// and cloned so it doesn't alias the parsed line's backing buffer. The JOIN
// prefix carries nick!user@host; extended-join adds <account> <realname>,
// account "*" when logged out.
func joinMember(m *ircv4.Message, nick string) Member {
	mem := Member{Nick: nick}
	if m.Prefix != nil {
		mem.User = clampRoster(m.Prefix.User)
		mem.Host = clampRoster(m.Prefix.Host)
	}
	if acct := m.Param(1); len(m.Params) >= 3 && acct != "*" {
		mem.Account = clampRoster(acct)
	}
	return mem
}

// memberJoin records a JOIN; ours creates the channel. extended-join
// (spec fetched 2026-07-16) carries <account> <realname>, account "*"
// when logged out. Caller holds r.mu.
func (r *roster) memberJoin(m *ircv4.Message, sender string, ours bool) {
	// Bound and detach the channel name before it becomes a map key and the
	// channelState.name: an unclamped ~64 KiB name (spoofed self-JOIN) would be
	// retained twice (folded key + raw name) across up to maxRosterChannels
	// entries. clampRoster caps to 512 B on a rune boundary and clones off the
	// parsed line. Applied once here so the key, name, and every Fold(ch) agree.
	ch := clampRoster(m.Param(0))
	st := r.joinChannel(ch, ours)
	if st == nil {
		return
	}
	// Clamp before folding so a ~64 KiB nick can't pin a full-length map key
	// (Fold allocates a same-length string); reuse it as the Member value.
	cs := clampRoster(sender)
	k := r.isup.Fold(cs)
	overBytes := r.totalBytes() >= maxRosterBytes
	old, known := st.members[k]
	if !known &&
		(len(st.members) >= maxChannelMembers || r.totalMembers() >= maxRosterMembers || overBytes) {
		return
	}
	mem := joinMember(m, cs)
	// A re-JOIN of a known member can grow its entry (fresh user/host/
	// account); over the byte budget, keep the existing entry instead so
	// replacement floods can't inflate past the ceiling.
	if known && overBytes && memberBytes(k, mem) > memberBytes(k, old) {
		return
	}
	st.put(st.members, k, mem)
	// If a NAMES accumulation is in flight for this channel, apply the live
	// join to the pending snapshot too, so the 366 swap does not revert it
	// (leaving a ghost/missing member). But a JOIN for an ALREADY-KNOWN member
	// skips the aggregate guard above, so only grow `pending` with a new key
	// while under the connection-wide budgets — otherwise a flood of re-JOINs
	// for known members could grow the in-flight snapshot unbounded.
	if st.pending != nil {
		if _, inPending := st.pending[k]; inPending || (r.totalMembers() < maxRosterMembers && !overBytes) {
			st.put(st.pending, k, mem)
		}
	}
}

// rename re-keys a nick in every channel, keeping its state. Caller
// holds r.mu.
func (r *roster) rename(from, to string) {
	fold := r.isup.Fold
	// Clamp before folding: the new key fold(to) must stay bounded (a hostile
	// ~64 KiB new nick would otherwise pin a full-length key), and clamping the
	// old nick keeps its fold in step with how it was stored.
	fromKey := fold(clampRoster(from))
	ct := clampRoster(to)
	toKey := fold(ct)
	// Over the byte budget, accept a rename only when it does not grow the
	// entry: NICK bypasses the admission guards, so a hostile server could
	// otherwise admit minimal members and then inflate each to a max-clamp
	// key+Nick via rename (same class updateEverywhere already guards).
	over := r.totalBytes() >= maxRosterBytes
	rekey := func(st *channelState, mp map[string]Member) {
		mem, ok := mp[fromKey]
		if !ok {
			return
		}
		renamed := mem
		renamed.Nick = ct
		if over && memberBytes(toKey, renamed) > memberBytes(fromKey, mem) {
			return // keep the old entry rather than inflate past the ceiling
		}
		st.del(mp, fromKey)
		st.put(mp, toKey, renamed)
	}
	for _, st := range r.chans {
		rekey(st, st.members)
		rekey(st, st.pending) // keep an in-flight NAMES snapshot consistent
	}
}

// updateEverywhere applies fn to nick's membership in every channel — the
// shape of nick-level facts (AWAY, ACCOUNT, WHOX, CHGHOST). It also updates the
// in-flight NAMES `pending` snapshot so a 366 swap arriving mid-update does not
// revert the change to stale data. Caller holds r.mu.
func (r *roster) updateEverywhere(nick string, fn func(Member) Member) {
	key := r.foldKey(nick)
	// Over the byte budget, accept only non-growing updates: field updates
	// bypass the admission guards, so a hostile server could otherwise admit
	// minimal members and then inflate every one to max-clamp fields.
	over := r.totalBytes() >= maxRosterBytes
	apply := func(st *channelState, mp map[string]Member) {
		mem, ok := mp[key]
		if !ok {
			return
		}
		nm := fn(mem)
		if over && memberBytes(key, nm) > memberBytes(key, mem) {
			return
		}
		st.put(mp, key, nm)
	}
	for _, st := range r.chans {
		apply(st, st.members)
		if st.pending != nil {
			apply(st, st.pending)
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
	acct = clampRoster(acct) // don't retain/overgrow a slice of the 354 line
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
// userhost-in-names, "@+nick!user@host" into its status prefixes, the bare
// nick, and the user/host (empty when not advertised). multi-prefix sends all
// prefixes "in order of 'rank', from highest to lowest" — keep them as-is.
func splitNamesPrefix(symbols, raw string) (prefixes, nick, user, host string) {
	i := 0
	for i < len(raw) && strings.IndexByte(symbols, raw[i]) != -1 {
		i++
	}
	prefixes, nick = raw[:i], raw[i:]
	if bang := strings.IndexByte(nick, '!'); bang != -1 {
		rest := nick[bang+1:]
		nick = nick[:bang]
		if at := strings.IndexByte(rest, '@'); at != -1 {
			user, host = rest[:at], rest[at+1:]
		} else {
			user = rest
		}
	}
	return prefixes, nick, user, host
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
	cls := r.isup.modeClassifier() // one ISUPPORT lock for the whole line
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
			switch cls.chanModeType(c) {
			case 'P': // status mode
				r.applyStatusMode(st, takeArg(), string(cls.symbolForMode(c)), adding)
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
	fk := r.foldKey(nick)
	// Apply to whichever map holds the member: during a NAMES burst the
	// member is in st.pending (not yet swapped into st.members), so a
	// MODE between 353 and 366 must land in pending to survive the swap.
	apply := func(mp map[string]Member) {
		mem, ok := mp[fk]
		if !ok {
			return
		}
		if adding {
			mem.Prefix = addPrefix(r.isup.PrefixSymbols(), mem.Prefix, sym)
		} else {
			mem.Prefix = strings.ReplaceAll(mem.Prefix, sym, "")
		}
		st.put(mp, fk, mem) // prefix growth is bounded by the PREFIX symbol set
	}
	apply(st.members)
	if st.pending != nil {
		apply(st.pending)
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
