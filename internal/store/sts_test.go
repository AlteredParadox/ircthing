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
	"errors"
	"math"
	"strconv"
	"testing"
	"time"
)

// A tampered record whose counters sit at MaxUint64 decodes cleanly, so it is
// enforced — and before the exhaustion-repair fix every Set/Clear failed
// forever, leaving the hostile policy pinned and unclearable. An authoritative
// mutation must instead re-mint fresh generations and proceed.
func TestSTSPolicyAuthoritativeMutationRepairsExhaustedGenerations(t *testing.T) {
	const host = "irc.example"
	maxUint := strconv.FormatUint(math.MaxUint64, 10)
	untilMs := strconv.FormatInt(time.Now().Add(time.Hour).UnixMilli(), 10)

	records := []struct {
		name  string
		key   string
		value string
		// active: the planted record still decodes as an enforced policy, so the
		// pre-mutation read must report it (not error, not absent) with the
		// planted revision. The legacy alias variant instead fails closed on read
		// (its migration hits the exhausted counter) but must still be repairable
		// by Set/Clear.
		active         bool
		activeRevision uint64
	}{
		{"canonical revision max", stsKey(host),
			`{"port":1,"until":` + untilMs + `,"revision":` + maxUint + `}`, true, math.MaxUint64},
		{"canonical epoch max", stsKey(host),
			`{"port":1,"until":` + untilMs + `,"revision":7,"policy_epoch":` + maxUint + `}`, true, 7},
		{"legacy alias revision max", "sts:IRC.Example.",
			`{"port":1,"until":` + untilMs + `,"revision":` + maxUint + `}`, false, 0},
	}

	newUntil := time.Now().Add(2 * time.Hour).Truncate(time.Millisecond)
	ops := []struct {
		name   string
		run    func(*Store) (uint64, uint64, error)
		verify func(*testing.T, *Store)
	}{
		{
			name: "set",
			run: func(s *Store) (uint64, uint64, error) {
				return s.SetSTSPolicy(ctx, host, 7000, newUntil, 2*time.Hour)
			},
			verify: func(t *testing.T, s *Store) {
				port, until, duration, _, _, ok, err := s.STSPolicy(ctx, host)
				if err != nil || !ok || port != 7000 || !until.Equal(newUntil) || duration != 2*time.Hour {
					t.Fatalf("after repair set: port=%d until=%v duration=%v ok=%v err=%v", port, until, duration, ok, err)
				}
			},
		},
		{
			name: "clear",
			run: func(s *Store) (uint64, uint64, error) {
				return s.ClearSTSPolicy(ctx, host)
			},
			verify: func(t *testing.T, s *Store) {
				_, _, _, _, _, ok, err := s.STSPolicy(ctx, host)
				if err != nil || ok {
					t.Fatalf("after repair clear: ok=%v err=%v", ok, err)
				}
			},
		},
	}

	for _, rec := range records {
		for _, op := range ops {
			t.Run(rec.name+"/"+op.name, func(t *testing.T) {
				s, _ := openTest(t, 10)
				if err := s.SetSetting(ctx, rec.key, rec.value); err != nil {
					t.Fatal(err)
				}

				port, _, _, revision, _, ok, err := s.STSPolicy(ctx, host)
				if rec.active {
					if err != nil || !ok || port != 1 || revision != rec.activeRevision {
						t.Fatalf("planted record not enforced: port=%d revision=%d ok=%v err=%v", port, revision, ok, err)
					}
				} else if err == nil {
					t.Fatalf("tampered alias read did not fail closed: ok=%v", ok)
				}

				gotRevision, gotEpoch, err := op.run(s)
				if err != nil {
					t.Fatalf("authoritative %s did not repair exhausted generations: %v", op.name, err)
				}
				// freshSTSGeneration mints in [2^62, 2^63); the mutation adds one.
				if gotRevision <= 1<<62 || gotEpoch <= 1<<62 || gotRevision == math.MaxUint64 || gotEpoch == math.MaxUint64 {
					t.Fatalf("generations not re-minted: revision=%d epoch=%d", gotRevision, gotEpoch)
				}
				op.verify(t, s)

				if rec.key != stsKey(host) {
					if _, present, err := s.SettingValue(ctx, rec.key); err != nil || present {
						t.Fatalf("tampered alias survived repair: present=%v err=%v", present, err)
					}
				}
			})
		}
	}
}

// A CAS reschedule is not authoritative (it does not come from a fresh
// verified-TLS policy advertisement) and must never repair an exhausted
// record — it keeps failing closed even when its expected revision matches.
func TestSTSPolicyRescheduleDoesNotRepairExhaustedGenerations(t *testing.T) {
	const host = "irc.example"
	s, _ := openTest(t, 10)
	untilMs := strconv.FormatInt(time.Now().Add(time.Hour).UnixMilli(), 10)
	planted := `{"port":1,"until":` + untilMs + `,"duration":3600000,"revision":` + strconv.FormatUint(math.MaxUint64, 10) + `}`
	if err := s.SetSetting(ctx, stsKey(host), planted); err != nil {
		t.Fatal(err)
	}

	_, applied, err := s.RescheduleSTSPolicy(ctx, host, math.MaxUint64, time.Now().Add(2*time.Hour))
	if !errors.Is(err, errSTSGenerationExhausted) || applied {
		t.Fatalf("reschedule of exhausted record: applied=%v err=%v", applied, err)
	}
	port, _, _, revision, _, ok, err := s.STSPolicy(ctx, host)
	if err != nil || !ok || port != 1 || revision != math.MaxUint64 {
		t.Fatalf("record changed by failed reschedule: port=%d revision=%d ok=%v err=%v", port, revision, ok, err)
	}
}

// A failed settings read must surface as an error, never as "no policy stored"
// (which would let the next connect downgrade to plaintext). True mid-iteration
// row faults cannot be injected without test hooks; this covers the propagation
// contract at the query layer.
func TestSTSPolicyReadErrorFailsClosed(t *testing.T) {
	const host = "irc.example"
	s, _ := openTest(t, 10)
	if _, _, err := s.SetSTSPolicy(ctx, host, 6697, time.Now().Add(time.Hour), time.Hour); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, _, _, _, ok, err := s.STSPolicy(canceled, host)
	if err == nil {
		t.Fatalf("canceled read reported success: ok=%v", ok)
	}
	if ok {
		t.Fatal("canceled read reported an enforceable policy")
	}
}
