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

package irc

import (
	"strings"
	"sync"

	ircv4 "gopkg.in/irc.v4"
)

// RPL_ISUPPORT (005) tracking. Parsing follows the modern spec
// (https://modern.ircdocs.horse/#rplisupport-parameters, fetched
// 2026-07-15): tokens are NAME, NAME=value, or -NAME (negation, reverting
// the parameter to its default); values use \xHH escapes. The parsed
// parameters drive behavior elsewhere: CASEMAPPING for every name
// comparison, CHANTYPES for channel detection, PREFIX and CHANMODES for
// roster tracking. Everything else stays accessible raw (MODES, TARGMAX,
// LINELEN consumers come later).
//
// Written by the connection's read loop, read by hub sessions: a mutex
// guards all state. reset() restores defaults at each registration.

const (
	defaultChanTypes     = "#&"
	defaultPrefixModes   = "ov"
	defaultPrefixSymbols = "@+"
	defaultCaseMapping   = "rfc1459"
)

// defaultChanModes is the RFC 1459 set: A=always-arg lists, B=always-arg,
// C=arg-when-set, D=never-arg.
var defaultChanModes = [4]string{"b", "k", "l", "imnpst"}

type isupport struct {
	mu            sync.Mutex
	raw           map[string]string
	chanTypes     string
	prefixModes   string // "qaohv"
	prefixSymbols string // "~&@%+", same order (rank: highest first)
	chanModes     [4]string
	caseMapping   string
	caseLocked    bool // first explicit CASEMAPPING wins; later changes ignored
}

func newISupport() *isupport {
	s := &isupport{}
	s.reset()
	return s
}

func (s *isupport) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.raw = make(map[string]string)
	s.caseLocked = false
	s.applyDefault("CHANTYPES")
	s.applyDefault("PREFIX")
	s.applyDefault("CHANMODES")
	s.applyDefault("CASEMAPPING")
}

// applyDefault restores one parameter's default. Caller holds s.mu.
func (s *isupport) applyDefault(name string) {
	switch name {
	case "CHANTYPES":
		s.chanTypes = defaultChanTypes
	case "PREFIX":
		s.prefixModes, s.prefixSymbols = defaultPrefixModes, defaultPrefixSymbols
	case "CHANMODES":
		s.chanModes = defaultChanModes
	case "CASEMAPPING":
		// Honor the one-time pin: once a mapping is locked (see applyToken),
		// a later "-CASEMAPPING" negation must NOT reset folding to the
		// default, or every already-folded map key would be stranded. reset()
		// clears caseLocked immediately before restoring defaults, so its own
		// path is unaffected.
		if !s.caseLocked {
			s.caseMapping = defaultCaseMapping
		}
	}
}

// handle consumes one RPL_ISUPPORT message; anything else is ignored.
// Tokens sit between the leading nick parameter and the trailing
// "are supported by this server".
func (s *isupport) handle(m *ircv4.Message) {
	if m.Command != "005" || len(m.Params) < 3 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, tok := range m.Params[1 : len(m.Params)-1] {
		if name, ok := strings.CutPrefix(tok, "-"); ok {
			delete(s.raw, name)
			s.applyDefault(name)
			continue
		}
		name, value, _ := strings.Cut(tok, "=")
		s.applyToken(name, unescapeValue(value))
	}
}

// applyToken records one ISUPPORT token, updating the parsed views of
// the parameters that drive behavior. Caller holds s.mu.
// maxISupportKeys bounds the ISUPPORT raw map against a server streaming
// endless distinct 005 tokens. Real servers advertise well under 50.
const maxISupportKeys = 256

// maxISupportValue bounds a single token's value length. The incoming line
// reader admits up to 16 KiB, so without this a hostile 005 could set an
// enormous PREFIX/CHANMODES that every MODE character then linearly scans
// (quadratic CPU). Real values are tens of bytes; 512 is generous.
const maxISupportValue = 512

// maxISupportKey bounds a token NAME. Real names are short words (CHANMODES,
// TARGMAX, …); an unbounded name would let 256 near-line-limit keys retain
// ~16 MiB per connection.
const maxISupportKey = 64

func (s *isupport) applyToken(name, value string) {
	if len(name) > maxISupportKey || len(value) > maxISupportValue {
		return // implausibly large — ignore, falling back to defaults
	}
	if _, known := s.raw[name]; !known && len(s.raw) >= maxISupportKeys {
		return // bound the map; ignore further new keys
	}
	// Clone both: they are substrings of the parsed 005 line (unescapeValue
	// returns its input unchanged when there is no escape), and even a short
	// retained substring pins the whole line's backing array. Cloning before
	// the switch detaches the parsed views (chanTypes, …) too.
	name, value = strings.Clone(name), strings.Clone(value)
	s.raw[name] = value
	switch name {
	case "CHANTYPES":
		s.chanTypes = value // empty is valid: no channels
	case "PREFIX":
		if modes, symbols, ok := parsePrefix(value); ok {
			s.prefixModes, s.prefixSymbols = modes, symbols
		}
	case "CHANMODES":
		parts := strings.SplitN(value, ",", 5)
		var cm [4]string
		for i := 0; i < len(parts) && i < 4; i++ {
			cm[i] = parts[i]
		}
		s.chanModes = cm
	case "CASEMAPPING":
		// The FIRST explicit mapping wins for the whole connection; a later
		// change is ignored. Folding semantics are baked into the roster/
		// NAMES/WHOX map KEYS, so flipping mid-session would silently strand
		// or collide existing entries. Real servers fix CASEMAPPING at
		// registration and never flip it, so pinning it is a cheap fail-safe
		// (cheaper than re-keying every folded map on a hostile flip).
		if s.caseLocked {
			break
		}
		switch value {
		case "rfc1459", "rfc1459-strict", "ascii":
			s.caseMapping = value
			s.caseLocked = true
		}
	}
}

// parsePrefix parses "(modes)symbols"; the two halves correspond
// positionally, ordered highest rank first. An empty value is valid (no
// prefixes at all).
func parsePrefix(value string) (modes, symbols string, ok bool) {
	if value == "" {
		return "", "", true
	}
	close := strings.IndexByte(value, ')')
	if !strings.HasPrefix(value, "(") || close == -1 {
		return "", "", false
	}
	modes, symbols = value[1:close], value[close+1:]
	if len(modes) != len(symbols) {
		return "", "", false
	}
	return modes, symbols, true
}

// unescapeValue decodes \xHH escapes (e.g. "\x20" for space).
func unescapeValue(v string) string {
	if !strings.Contains(v, `\x`) {
		return v
	}
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		if v[i] == '\\' && i+3 < len(v) && v[i+1] == 'x' {
			if hi, ok1 := hexVal(v[i+2]); ok1 {
				if lo, ok2 := hexVal(v[i+3]); ok2 {
					b.WriteByte(hi<<4 | lo)
					i += 3
					continue
				}
			}
		}
		b.WriteByte(v[i])
	}
	return b.String()
}

func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// Raw returns a parameter's raw (unescaped) value.
func (s *isupport) Raw(name string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.raw[name]
	return v, ok
}

// IsChannel reports whether target names a channel per CHANTYPES.
func (s *isupport) IsChannel(target string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return target != "" && strings.IndexByte(s.chanTypes, target[0]) != -1
}

// ChanTypes returns the channel prefix characters.
func (s *isupport) ChanTypes() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.chanTypes
}

// PrefixSymbols returns the status prefix characters, highest rank first.
func (s *isupport) PrefixSymbols() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.prefixSymbols
}

// SymbolForMode maps a status mode letter ('o') to its prefix char ("@"),
// or "" if the letter is not a status mode.
func (s *isupport) SymbolForMode(mode byte) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := strings.IndexByte(s.prefixModes, mode); i != -1 {
		return string(s.prefixSymbols[i])
	}
	return ""
}

// ChanModeType classifies a channel mode letter: 'A' (list, always an
// argument), 'B' (always an argument), 'C' (argument only when setting),
// 'D' (never), 'P' (status/prefix mode, always an argument), or 0 for
// unknown letters.
func (s *isupport) ChanModeType(mode byte) byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.IndexByte(s.prefixModes, mode) != -1 {
		return 'P'
	}
	for i, set := range s.chanModes {
		if strings.IndexByte(set, mode) != -1 {
			return byte('A' + i)
		}
	}
	return 0
}

// modeClassifier is a byte-indexed snapshot of the ISUPPORT PREFIX/CHANMODES
// tables. Building it takes the ISUPPORT lock once; classifying each MODE byte
// is then an O(1) array lookup — so a hostile 64 KiB MODE line no longer costs
// a mutex cycle plus a linear ISUPPORT scan per byte (see applyChannelMode).
type modeClassifier struct {
	class [256]byte // mode letter -> 'A'/'B'/'C'/'D' or 'P' (status); 0 unknown
	sym   [256]byte // status letter -> its prefix symbol; 0 if not a status mode
}

func (c *modeClassifier) chanModeType(mode byte) byte  { return c.class[mode] }
func (c *modeClassifier) symbolForMode(mode byte) byte { return c.sym[mode] }

// modeClassifier returns a lock-free byte-table snapshot mirroring the
// precedence of ChanModeType/SymbolForMode: PREFIX status modes win 'P' (and
// carry a symbol), and among CHANMODES sets the earliest (A<B<C<D) wins.
func (s *isupport) modeClassifier() modeClassifier {
	s.mu.Lock()
	defer s.mu.Unlock()
	var mc modeClassifier
	for i := 0; i < len(s.prefixModes); i++ {
		mc.class[s.prefixModes[i]] = 'P'
		if i < len(s.prefixSymbols) {
			mc.sym[s.prefixModes[i]] = s.prefixSymbols[i]
		}
	}
	for i, set := range s.chanModes {
		for j := 0; j < len(set); j++ {
			if mc.class[set[j]] == 0 { // don't override a status ('P') mode
				mc.class[set[j]] = byte('A' + i)
			}
		}
	}
	return mc
}

// Fold lowercases a name per CASEMAPPING for comparison: rfc1459 treats
// {}|^ as the lowercase of []\~ (RFC 2812 §2.2), rfc1459-strict omits
// the ^~ pair, ascii maps A-Z only.
func (s *isupport) Fold(str string) string {
	s.mu.Lock()
	cm := s.caseMapping
	s.mu.Unlock()
	b := []byte(str)
	for i, c := range b {
		switch {
		case c >= 'A' && c <= 'Z':
			b[i] = c + 32
		case cm == "ascii":
		case c == '[':
			b[i] = '{'
		case c == ']':
			b[i] = '}'
		case c == '\\':
			b[i] = '|'
		case c == '~' && cm == "rfc1459":
			b[i] = '^'
		}
	}
	return string(b)
}

// FoldEqual reports whether two names are the same entity under the
// current casemapping.
func (s *isupport) FoldEqual(a, b string) bool {
	return s.Fold(a) == s.Fold(b)
}
