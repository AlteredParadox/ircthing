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

package store

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"strings"
	"time"
)

// STS policy persistence on the settings table (key "sts:<host>", value JSON
// containing the port, expiry, duration, mutation revision, and semantic
// policy epoch), so a server's
// upgrade-to-TLS policy — and the duration needed to reschedule its expiry on
// disconnect — survives restarts. Implements irc.STSStore.
//
// until and duration are POINTERS so an absent field is distinguishable from a
// zero one: a record missing (or zero/negative) `until` is corrupt, not an
// "expired-at-epoch" policy that would silently permit a plaintext downgrade.
type stsRecord struct {
	Port        int    `json:"port,omitempty"`
	Until       *int64 `json:"until,omitempty"`
	Duration    *int64 `json:"duration,omitempty"` // absent in pre-existing records
	Revision    uint64 `json:"revision,omitempty"`
	PolicyEpoch uint64 `json:"policy_epoch,omitempty"`
	Disabled    bool   `json:"disabled,omitempty"` // preserves both generations on clear
}

// errCorruptSTSPolicy distinguishes semantic record corruption from database
// I/O failures. Reads and CAS reschedules fail closed on it. A fresh policy SET
// or CLEAR received over an already-verified TLS connection may repair it.
var errCorruptSTSPolicy = errors.New("sts: corrupt policy record")

func corruptSTSPolicy(host string) error {
	return fmt.Errorf("%w for %q", errCorruptSTSPolicy, host)
}

func canonicalSTSHost(host string) string {
	host = strings.TrimSuffix(strings.TrimSpace(host), ".")
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.Unmap().String()
	}
	return strings.ToLower(host)
}

func stsKey(host string) string { return "sts:" + canonicalSTSHost(host) }

type stsAlias struct{ key, value string }

// stsAliasesLocked returns every raw-key spelling equivalent to host. Caller
// holds s.mu. Keeping the raw values is necessary both for fail-closed alias
// validation and for atomically deleting corrupt legacy aliases during repair.
func (s *Store) stsAliasesLocked(ctx context.Context, host string) ([]stsAlias, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings WHERE key LIKE 'sts:%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var aliases []stsAlias
	for rows.Next() {
		var a stsAlias
		if err := rows.Scan(&a.key, &a.value); err != nil {
			return nil, err
		}
		if canonicalSTSHost(strings.TrimPrefix(a.key, "sts:")) == canonicalSTSHost(host) {
			aliases = append(aliases, a)
		}
	}
	// A mid-iteration failure (driver error, context cancellation) makes Next
	// return false with the rows already auto-closed and Close returning nil;
	// only Err reports it. Without this check a truncated — possibly empty —
	// alias list would be returned as success, read upstream as "no policy
	// stored", permitting exactly the plaintext downgrade this file prevents.
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return aliases, nil
}

// stsValueLocked reads the canonical row. For backward compatibility with
// releases that keyed STS by the config's raw hostname, it also discovers all
// case/trailing-dot/IP-spelling aliases, validates them fail-closed, and
// migrates the strongest active policy to the canonical key. Caller holds
// s.mu. This prevents an upgrade learned before canonicalization from being
// missed (and the next connection silently downgrading to plaintext).
func (s *Store) stsValueLocked(ctx context.Context, host string) (string, bool, error) {
	key := stsKey(host)
	aliases, err := s.stsAliasesLocked(ctx, host)
	if err != nil {
		return "", false, err
	}
	if len(aliases) == 0 {
		return "", false, nil
	}

	// Every matching row — including a canonical one — must be readable. A
	// corrupt alias might be the real active policy; ignoring it would fail open.
	// Legacy revisions were independently generated under each raw key and are
	// therefore not comparable. Security state wins: any unexpired policy beats
	// an expired/disabled one, then the furthest expiry wins.
	var chosen stsRecord
	var maxRevision uint64
	chosenActive := false
	chosenUntil := int64(0)
	now := time.Now()
	for i, a := range aliases {
		_, until, _, revision, _, ok, err := decodeSTSPolicy(canonicalSTSHost(host), a.value, true)
		if err != nil {
			return "", false, err
		}
		var rec stsRecord
		_ = json.Unmarshal([]byte(a.value), &rec) // decodeSTSPolicy proved JSON
		if revision > maxRevision {
			maxRevision = revision
		}
		active := ok && now.Before(until)
		candidateUntil := int64(0)
		if ok {
			candidateUntil = until.UnixMilli()
		}
		if i == 0 || (active && !chosenActive) ||
			(active == chosenActive && candidateUntil > chosenUntil) {
			chosen = rec
			chosenActive = active
			chosenUntil = candidateUntil
		}
	}
	// A single already-canonical row needs no rewrite (and must not bump its
	// generation on every read). The full scan above still checked for corrupt
	// or active aliases before taking this fast path.
	if len(aliases) == 1 && aliases[0].key == key {
		return aliases[0].value, true, nil
	}
	next, err := nextSTSRevision(maxRevision)
	if err != nil {
		return "", false, err
	}
	chosen.Revision = next
	// Migration is a storage mutation, not a new policy advertised by the
	// server. Preserve the chosen record's semantic epoch; only Set/Clear advance
	// it. The CAS revision still advances so a manager cannot overwrite this
	// consolidated record with an expiry computed from a pre-migration alias.
	b, err := json.Marshal(chosen)
	if err != nil {
		return "", false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, string(b)); err == nil {
		for _, a := range aliases {
			if a.key != key {
				_, err = tx.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, a.key)
				if err != nil {
					break
				}
			}
		}
	}
	if err != nil {
		_ = tx.Rollback()
		return "", false, err
	}
	if err := tx.Commit(); err != nil {
		return "", false, err
	}
	return string(b), true, nil
}

func ptrInt64(v int64) *int64 { return &v }

// STSPolicy returns the stored policy for host; ok is false when NONE is stored
// (absent row). A present row that is empty, malformed, or missing a valid port
// or a positive `until` is CORRUPT — returned as an error so the caller fails
// closed (refuses a plaintext downgrade) rather than treating it as absent or
// as expired-at-epoch. A positive `until` in the past is a legitimately expired
// policy (ok, with the past time); the caller decides staleness. duration is 0
// when the record predates duration persistence.
func decodeSTSPolicy(host, v string, present bool) (port int, until time.Time, duration time.Duration, revision, policyEpoch uint64, ok bool, err error) {
	if !present {
		return 0, time.Time{}, 0, 0, 0, false, nil
	}
	var rec stsRecord
	if json.Unmarshal([]byte(v), &rec) != nil {
		return 0, time.Time{}, 0, 0, 0, false, corruptSTSPolicy(host)
	}
	if rec.Disabled {
		if rec.Revision == 0 {
			return 0, time.Time{}, 0, 0, 0, false, corruptSTSPolicy(host)
		}
		return 0, time.Time{}, 0, rec.Revision, rec.PolicyEpoch, false, nil
	}
	if rec.Port <= 0 || rec.Port > 65535 || rec.Until == nil || *rec.Until <= 0 {
		return 0, time.Time{}, 0, rec.Revision, rec.PolicyEpoch, false, corruptSTSPolicy(host)
	}
	// Duration affects rescheduling, not whether the policy is enforced. Invalid
	// legacy/tampered values disable rescheduling without disabling `until`.
	if rec.Duration != nil && *rec.Duration >= 1 && *rec.Duration <= maxSTSDurationMs {
		duration = time.Duration(*rec.Duration) * time.Millisecond
	}
	return rec.Port, time.UnixMilli(*rec.Until), duration, rec.Revision, rec.PolicyEpoch, true, nil
}

func (s *Store) STSPolicy(ctx context.Context, host string) (port int, until time.Time, duration time.Duration, revision, policyEpoch uint64, ok bool, err error) {
	host = canonicalSTSHost(host)
	s.mu.Lock()
	defer s.mu.Unlock()
	v, present, err := s.stsValueLocked(ctx, host)
	if err != nil {
		return 0, time.Time{}, 0, 0, 0, false, err
	}
	return decodeSTSPolicy(host, v, present)
}

// maxSTSDurationMs caps a stored STS duration at ~100 years in milliseconds —
// far longer than any real policy, and small enough that the ms→ns conversion
// (×1e6) stays well within int64. Mirrors the parse-time clamp in internal/irc.
const maxSTSDurationMs = int64(100*365*24*60*60) * 1000

// errSTSGenerationExhausted marks a revision/policy-epoch counter that cannot
// be incremented. Healthy counters never get near MaxUint64 (freshSTSGeneration
// yields values below 2^63 and each mutation adds one), so this shape only
// arises from a tampered or corrupted record — the same threat model as
// errCorruptSTSPolicy, and it gets the same treatment: reads and CAS
// reschedules fail closed; an authoritative Set/Clear repairs by minting fresh
// generations. Failing forever instead would leave the tampered policy
// enforced AND unclearable — a permanently wedged network.
var errSTSGenerationExhausted = errors.New("sts: policy revision exhausted")

func nextSTSRevision(current uint64) (uint64, error) {
	if current == math.MaxUint64 {
		return 0, errSTSGenerationExhausted
	}
	return current + 1, nil
}

// freshSTSGeneration returns an opaque high-range generation used only when a
// corrupt row no longer exposes its previous counters. Resetting to zero would
// create an ABA: a manager holding an ancient revision could then pass a CAS
// and resurrect stale state. A random 62-bit value in [2^62,2^63) makes that
// collision cryptographically negligible while leaving enormous increment
// headroom. If entropy is unavailable, repair fails closed.
func freshSTSGeneration() (uint64, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, fmt.Errorf("sts: generate repair generation: %w", err)
	}
	return (binary.LittleEndian.Uint64(b[:]) & ((uint64(1) << 62) - 1)) | (uint64(1) << 62), nil
}

// writeSTSRecordLocked writes the canonical record and, during corruption
// repair, removes every noncanonical equivalent alias in the SAME transaction.
// Leaving one corrupt alias behind would make the next fail-closed read brick
// the network again. Caller holds s.mu.
func (s *Store) writeSTSRecordLocked(ctx context.Context, host string, rec stsRecord, repairAliases []stsAlias) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	key := stsKey(host)
	if len(repairAliases) == 0 {
		_, err = s.db.ExecContext(ctx,
			`INSERT INTO settings (key, value) VALUES (?, ?)
			 ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, string(b))
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, string(b)); err == nil {
		for _, a := range repairAliases {
			if a.key != key {
				if _, err = tx.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, a.key); err != nil {
					break
				}
			}
		}
	}
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// updateSTS serializes read-modify-write with every other store operation.
// Disabled policies are tombstones rather than deleted rows, preventing an
// ABA where a stale generation-zero manager resurrects a cleared policy. An
// unconditional Set/Clear is authoritative because it came from a verified
// TLS connection, so it may replace corrupt or exhaustion-tampered rows; a
// CAS reschedule never may.
func (s *Store) updateSTS(ctx context.Context, host string, expected *uint64, makeRecord func(uint64, stsRecord, bool) (stsRecord, bool, error)) (revision, policyEpoch uint64, applied bool, err error) {
	host = canonicalSTSHost(host)
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, present, err := s.stsValueLocked(ctx, host)
	var repairAliases []stsAlias
	var current stsRecord
	if err != nil {
		if expected != nil || !(errors.Is(err, errCorruptSTSPolicy) || errors.Is(err, errSTSGenerationExhausted)) {
			return 0, 0, false, err
		}
		// The semantic record is unreadable — or legacy-alias migration hit an
		// exhausted (tampered) counter — but a fresh Set/Clear can safely
		// replace it. Gather every equivalent spelling for atomic deletion, then
		// start from an opaque generation rather than zero (ABA protection).
		repairAliases, err = s.stsAliasesLocked(ctx, host)
		if err != nil {
			return 0, 0, false, err
		}
		freshRevision, err := freshSTSGeneration()
		if err != nil {
			return 0, 0, false, err
		}
		freshEpoch, err := freshSTSGeneration()
		if err != nil {
			return 0, 0, false, err
		}
		current = stsRecord{Revision: freshRevision, PolicyEpoch: freshEpoch}
		present = len(repairAliases) > 0
	} else if present {
		// stsValueLocked already validated this JSON (and migrated aliases).
		if err := json.Unmarshal([]byte(raw), &current); err != nil {
			return 0, 0, false, corruptSTSPolicy(host)
		}
	}
	if expected != nil && current.Revision != *expected {
		return current.Revision, current.PolicyEpoch, false, nil
	}
	// A record that decodes cleanly but carries a counter at MaxUint64 is
	// exhaustion-tampered (see errSTSGenerationExhausted): incrementing it in
	// makeRecord would fail every future Set/Clear, wedging the network on
	// whatever the record pins. An authoritative mutation re-mints both
	// generations, exactly as for an undecodable record. A CAS reschedule
	// (expected != nil) is skipped here and keeps failing closed inside
	// makeRecord, so healthy optimistic concurrency is untouched.
	if expected == nil && (current.Revision == math.MaxUint64 || current.PolicyEpoch == math.MaxUint64) {
		freshRevision, err := freshSTSGeneration()
		if err != nil {
			return 0, 0, false, err
		}
		freshEpoch, err := freshSTSGeneration()
		if err != nil {
			return 0, 0, false, err
		}
		current.Revision = freshRevision
		current.PolicyEpoch = freshEpoch
	}
	next, apply, err := makeRecord(current.Revision, current, present)
	if err != nil || !apply {
		return current.Revision, current.PolicyEpoch, false, err
	}
	if err := s.writeSTSRecordLocked(ctx, host, next, repairAliases); err != nil {
		return 0, 0, false, err
	}
	return next.Revision, next.PolicyEpoch, true, nil
}

func (s *Store) SetSTSPolicy(ctx context.Context, host string, port int, until time.Time, duration time.Duration) (revision, policyEpoch uint64, err error) {
	if port <= 0 || port > 65535 || until.UnixMilli() <= 0 {
		return 0, 0, errors.New("sts: invalid policy")
	}
	// Clamp what we WRITE to the same valid range we accept on read, so a
	// caller bug can never persist a duration that read-back would discard
	// (silently losing rescheduling) or that would overflow.
	ms := duration.Milliseconds()
	if ms < 1 {
		ms = 1
	} else if ms > maxSTSDurationMs {
		ms = maxSTSDurationMs
	}
	revision, policyEpoch, _, err = s.updateSTS(ctx, host, nil, func(current uint64, rec stsRecord, _ bool) (stsRecord, bool, error) {
		nextRevision, err := nextSTSRevision(current)
		if err != nil {
			return stsRecord{}, false, err
		}
		nextEpoch, err := nextSTSRevision(rec.PolicyEpoch)
		return stsRecord{Port: port, Until: ptrInt64(until.UnixMilli()), Duration: ptrInt64(ms), Revision: nextRevision, PolicyEpoch: nextEpoch}, true, err
	})
	return revision, policyEpoch, err
}

func (s *Store) ClearSTSPolicy(ctx context.Context, host string) (revision, policyEpoch uint64, err error) {
	revision, policyEpoch, _, err = s.updateSTS(ctx, host, nil, func(current uint64, rec stsRecord, _ bool) (stsRecord, bool, error) {
		nextRevision, err := nextSTSRevision(current)
		if err != nil {
			return stsRecord{}, false, err
		}
		nextEpoch, err := nextSTSRevision(rec.PolicyEpoch)
		return stsRecord{Revision: nextRevision, PolicyEpoch: nextEpoch, Disabled: true}, true, err
	})
	return revision, policyEpoch, err
}

func (s *Store) RescheduleSTSPolicy(ctx context.Context, host string, expectedRevision uint64, until time.Time) (revision uint64, applied bool, err error) {
	revision, _, applied, err = s.updateSTS(ctx, host, &expectedRevision, func(current uint64, rec stsRecord, present bool) (stsRecord, bool, error) {
		if !present || rec.Disabled {
			return stsRecord{}, false, nil
		}
		if rec.Port <= 0 || rec.Port > 65535 || rec.Until == nil || *rec.Until <= 0 {
			return stsRecord{}, false, corruptSTSPolicy(canonicalSTSHost(host))
		}
		if rec.Duration == nil || *rec.Duration < 1 || *rec.Duration > maxSTSDurationMs {
			return stsRecord{}, false, nil
		}
		next, err := nextSTSRevision(current)
		if err != nil {
			return stsRecord{}, false, err
		}
		rec.Until = ptrInt64(until.UnixMilli())
		rec.Revision = next
		return rec, true, nil
	})
	return revision, applied, err
}
