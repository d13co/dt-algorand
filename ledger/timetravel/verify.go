// Copyright (C) 2019-2026 Algorand Foundation Ltd.
// This file is part of go-algorand
//
// go-algorand is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as
// published by the Free Software Foundation, either version 3 of the
// License, or (at your option) any later version.
//
// go-algorand is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with go-algorand.  If not, see <https://www.gnu.org/licenses/>.

package timetravel

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/cockroachdb/pebble/v2"
)

// ErrDiverged is wrapped by the errors of Verify: the stored round differs
// from the one the caller evaluated.
var ErrDiverged = errors.New("timetravel: the stored history differs from the evaluated round")

// Verify checks a round the store already holds against the same round as
// the caller evaluated it: the header, the digest of the changes, the
// expired stake, and the stored value of every changed object. A store
// installed from elsewhere (a snapshot) is checked this way as the ledger
// replays the rounds it already holds.
//
// A missing expired stake record is not an error, since builds before the
// records existed did not write them. The value check sees every object the
// round changed; it cannot see objects the store changed in that round but
// the evaluation did not.
func (s *Store) Verify(rnd uint64, changes []Change, hdr []byte, expired *uint64) error {
	return diverged(s.verify(rnd, changes, hdr, expired))
}

// diverged reports a record the store can't decode as a difference: a store
// from elsewhere must not get past the checks by holding garbage.
func diverged(err error) error {
	if errors.Is(err, ErrCorrupt) && !errors.Is(err, ErrDiverged) {
		return fmt.Errorf("%w: %w", ErrDiverged, err)
	}
	return err
}

func (s *Store) verify(rnd uint64, changes []Change, hdr []byte, expired *uint64) error {
	if err := s.acquire(); err != nil {
		return err
	}
	defer s.closeMu.RUnlock()
	if last, ok := s.Last(); !ok || rnd > last {
		return fmt.Errorf("timetravel: round %d is not stored", rnd)
	}

	rec, ok, err := s.headerRecordLocked(rnd)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: round %d has no header record", ErrDiverged, rnd)
	}
	if !bytes.Equal(rec[DigestLen:], hdr) {
		return fmt.Errorf("%w: round %d has a different header", ErrDiverged, rnd)
	}
	stored, found, err := s.expiredStakeLocked(rnd)
	if err != nil {
		return err
	}
	switch {
	case found && expired == nil:
		return fmt.Errorf("%w: round %d has an expired stake record the evaluation has none for", ErrDiverged, rnd)
	case found && stored != *expired:
		return fmt.Errorf("%w: round %d has expired stake %d, evaluated %d", ErrDiverged, rnd, stored, *expired)
	}

	return s.verifyChangesLocked(rec, rnd, changes)
}

// VerifyChanges checks a stored round's changes alone, as Verify does: the
// digest and the stored value of every changed object. Tools use it with
// deltas from elsewhere (an archival node's /v2/deltas).
func (s *Store) VerifyChanges(rnd uint64, changes []Change) error {
	if err := s.acquire(); err != nil {
		return err
	}
	defer s.closeMu.RUnlock()
	rec, ok, err := s.headerRecordLocked(rnd)
	if err != nil {
		return diverged(err)
	}
	if !ok {
		return fmt.Errorf("%w: round %d has no header record", ErrDiverged, rnd)
	}
	return diverged(s.verifyChangesLocked(rec, rnd, changes))
}

func (s *Store) verifyChangesLocked(rec []byte, rnd uint64, changes []Change) error {
	if !bytes.Equal(rec[:DigestLen], ChangesDigest(changes)) {
		return fmt.Errorf("%w: round %d has a different changes digest", ErrDiverged, rnd)
	}
	// an object changed twice in a round ends at its last change, as AddRound
	// writes them in order
	seen := make(map[string]struct{}, len(changes))
	for i := len(changes) - 1; i >= 0; i-- {
		ch := changes[i]
		if _, dup := seen[string(ch.Key)]; dup {
			continue
		}
		seen[string(ch.Key)] = struct{}{}
		val, ok, err := s.get(ch.Key, rnd)
		if err != nil {
			return err
		}
		if !sameState(val, ok, ch.Value, !ch.Deleted) {
			return fmt.Errorf("%w: round %d has a different value of %x", ErrDiverged, rnd, ch.Key)
		}
	}
	return nil
}

// keyDivergedMeta marks a store that Verify found to differ from the chain:
// the reason, as text. Nothing removes it; the store must be replaced.
var keyDivergedMeta = []byte("md")

// MarkDiverged records durably that the store differs from the chain, so that
// the history stays untrusted across restarts. The first reason is kept.
func (s *Store) MarkDiverged(reason string) error {
	if err := s.acquire(); err != nil {
		return err
	}
	defer s.closeMu.RUnlock()
	if s.readOnly {
		return ErrReadOnly
	}
	if _, found, err := s.divergedLocked(); err != nil || found {
		return err
	}
	return s.db.Set(keyDivergedMeta, []byte(reason), pebble.Sync)
}

// ClearDiverged removes the mark MarkDiverged left, for an operator who
// found the difference to be harmless or fixed it.
func (s *Store) ClearDiverged() error {
	if err := s.acquire(); err != nil {
		return err
	}
	defer s.closeMu.RUnlock()
	if s.readOnly {
		return ErrReadOnly
	}
	return s.db.Delete(keyDivergedMeta, pebble.Sync)
}

// Diverged returns the reason MarkDiverged recorded, if any.
func (s *Store) Diverged() (string, bool, error) {
	if err := s.acquire(); err != nil {
		return "", false, err
	}
	defer s.closeMu.RUnlock()
	return s.divergedLocked()
}

func (s *Store) divergedLocked() (string, bool, error) {
	v, closer, err := s.db.Get(keyDivergedMeta)
	if errors.Is(err, pebble.ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	defer closer.Close()
	return string(v), true, nil
}

// Checkpoint writes a consistent copy of the store to dir, which must not
// exist, while the store stays in use: Pebble hard-links the store's files
// when dir is on the same filesystem and copies them otherwise. The memtable
// is flushed first, so the copy needs no WAL replay to hold every round.
func (s *Store) Checkpoint(dir string) error {
	if err := s.acquire(); err != nil {
		return err
	}
	defer s.closeMu.RUnlock()
	return s.db.Checkpoint(dir, pebble.WithFlushedWAL())
}
