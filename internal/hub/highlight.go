// ircthing — a self-hosted, always-connected web IRC client.
// Copyright (C) 2026 AlteredParadox
//
// This program is free software: you can redistribute it and/or modify it
// under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or (at your
// option) any later version.
//
// This program is distributed in the hope that it will be useful, but WITHOUT
// ANY WARRANTY; without even the implied warranty of MERCHANTABILITY or
// FITNESS FOR A PARTICULAR PURPOSE. See the GNU Affero General Public License
// for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program. If not, see <https://www.gnu.org/licenses/>.

package hub

import (
	"regexp"
	"strings"
)

// Server-side highlight detection for Web Push: a message that would
// highlight in the browser must ALSO highlight here, or a backgrounded
// phone misses the ping. These are 1:1 ports of the client's matcher
// (web/src/irc.js mentionsMe/stripFormatting/foldNick and
// web/src/notify.js highlightText); the two test suites share their case
// tables so the semantics cannot drift apart silently. Any change here
// needs the mirror change there, and vice versa.

// Rule is one user highlight rule from the synced "highlight_rules"
// setting: a case-insensitive substring, optionally scoped to a network
// (empty Network = all). IDs key the settings-UI rows; the server only
// round-trips them.
type Rule struct {
	Pattern string `json:"pattern"`
	Network string `json:"network"`
	ID      string `json:"id"`
}

// stripCodes removes mIRC formatting: three disjoint-by-lead-byte passes
// (indexed colour with optional args, hex colour with optional args, bare
// attribute bytes) — web/src/irc.js stripFormatting. The \x03 form
// consumes ",BG" ONLY after a foreground digit: a bare "\x03,5" is a
// colour reset plus the literal ",5", which must survive so matching
// agrees with what the body renders.
var (
	stripIndexed = regexp.MustCompile(`\x03(?:[0-9]{1,2}(?:,[0-9]{1,2})?)?`)
	stripHex     = regexp.MustCompile(`\x04(?:[0-9a-fA-F]{6}(?:,[0-9a-fA-F]{6})?)?`)
	stripAttrs   = regexp.MustCompile("[\x02\x0f\x11\x16\x1d\x1e\x1f]")
)

func stripCodes(text string) string {
	text = stripIndexed.ReplaceAllString(text, "")
	text = stripHex.ReplaceAllString(text, "")
	return stripAttrs.ReplaceAllString(text, "")
}

// foldNick lowercases and applies the rfc1459 casemapping equivalences
// (RFC 2812 §2.2: {}|^ ≡ []\~) — the same loose-superset fold the client
// makes (web/src/irc.js foldNick): we do not know the network's
// CASEMAPPING at match time, and a missed ping on an rfc1459 network is
// worse than a rare false-positive highlight on a strict-ascii one.
var foldNickReplacer = strings.NewReplacer("[", "{", "]", "}", `\`, "|", "~", "^")

func foldNick(s string) string {
	return foldNickReplacer.Replace(strings.ToLower(s))
}

// nickBoundary is the complement of the IRC nickname alphabet: \b is
// wrong for nicks (nick_, nick[] ...), so word boundaries are "anything
// that cannot be part of a nick". Folded text contains {}|^ where the
// original had []\~ — both sets are listed, so they stay nick characters.
const nickBoundary = "[^A-Za-z0-9_\\-\\[\\]\\\\`\\^{}|]"

// mentionsMe reports whether text addresses nick as a whole word,
// after stripping formatting (a colour code between characters must not
// hide a mention) and folding both sides.
func mentionsMe(text, nick string) bool {
	if nick == "" {
		return false
	}
	re, err := regexp.Compile(`(?i)(^|` + nickBoundary + `)` + regexp.QuoteMeta(foldNick(nick)) + `($|` + nickBoundary + `)`)
	if err != nil {
		return false
	}
	return re.MatchString(foldNick(stripCodes(text)))
}

// highlightText reports whether a message should highlight: it mentions
// our nick, or matches a rule scoped to this network (or global). The
// caller handles self-exclusion and the PM-always-alerts policy.
func highlightText(text, nick string, rules []Rule, network string) bool {
	if text == "" {
		return false
	}
	// Strip first: a colour code inside a keyword ("de\x02ploy") renders
	// invisibly but would defeat a substring rule.
	clean := stripCodes(text)
	if mentionsMe(clean, nick) {
		return true
	}
	lower := strings.ToLower(clean)
	for _, r := range rules {
		if r.Pattern == "" {
			continue
		}
		if r.Network != "" && r.Network != network {
			continue
		}
		if strings.Contains(lower, strings.ToLower(r.Pattern)) {
			return true
		}
	}
	return false
}
