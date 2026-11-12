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

package ledger

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/ledger/timetravel"
)

// ttChange is a change the change lookups must find, from the versions oracle.
type ttChange struct {
	rnd    basics.Round
	status ChangeStatus
	kv     *basics.TealValue
	name   []byte
}

// ttChangeLookup is a change lookup with a byte budget (0: none).
type ttChangeLookup func(from, to basics.Round, budget uint64) (Change, error)

// checkChangeLookups compares lookup with chs, the changes in round order (a round's in name
// order), over every range between rounds around the changes, in both directions, unbounded and
// with a budget of a byte, resuming from each stop. Of a round's changes, the first is found in
// either direction.
func checkChangeLookups(t *testing.T, latest basics.Round, chs []ttChange, lookup ttChangeLookup, msg string) {
	t.Helper()
	sample := []basics.Round{0, latest}
	for _, c := range chs {
		for _, r := range []basics.Round{c.rnd - 1, c.rnd, c.rnd + 1} {
			if r <= latest {
				sample = append(sample, r)
			}
		}
	}
	slices.Sort(sample)
	sample = slices.Compact(sample)
	for _, from := range sample {
		for _, to := range sample {
			var want ttChange
			for _, c := range chs {
				if from <= to && c.rnd >= from && c.rnd <= to {
					want = c
					break
				}
				if from > to && c.rnd >= to && c.rnd <= from && (want.status == ChangeNone || c.rnd > want.rnd) {
					want = c // the first of the last round
				}
			}
			for _, budget := range []uint64{0, 1} {
				start := from
				c, err := lookup(start, to, budget)
				for err == nil && c.Status == ChangeMore {
					require.Positive(t, budget, msg)
					if from <= to {
						require.Greater(t, c.Round, start, msg)
						require.LessOrEqual(t, c.Round, to, msg)
					} else {
						require.Less(t, c.Round, start, msg)
						require.GreaterOrEqual(t, c.Round, to, msg)
					}
					start = c.Round
					c, err = lookup(start, to, budget)
				}
				require.NoError(t, err, msg)
				if c.Status >= ChangeCreated && c.Status <= ChangeDeleted {
					require.Positive(t, c.Versions, "%s: a change is a version examined", msg)
				}
				got := ttChange{rnd: c.Round, status: c.Status, kv: c.KeyValue, name: c.Name}
				require.Equal(t, want, got, "%s from %d to %d budget %d", msg, from, to, budget)
			}
		}
	}
}

// keyChanges turns the versions of a state key, as a Key page lists them, into its changes.
func keyChanges(vs []Version) []ttChange {
	var chs []ttChange
	var before *basics.TealValue
	for _, v := range vs {
		var kv *basics.TealValue
		if len(v.Apps) > 0 {
			kv = v.Apps[0].KeyValue
		} else {
			kv = v.LocalStates[0].KeyValue
		}
		chs = append(chs, ttChange{rnd: v.Round, status: changeStatus(kv != nil, before != nil), kv: kv})
		before = kv
	}
	return chs
}

// objectChanges turns the versions of one object into its changes: deleted if the version deletes
// it, created if it did not exist at the version before.
func objectChanges(vs []Version, deleted func(v Version) bool) []ttChange {
	var chs []ttChange
	exists := false
	for _, v := range vs {
		ok := !deleted(v)
		chs = append(chs, ttChange{rnd: v.Round, status: changeStatus(ok, exists)})
		exists = ok
	}
	return chs
}

// checkKeyChanges checks StateKeyChange, and ObjectChange of the states, against the versions
// lookups of the state keys and states checkKeyVersions checks.
func checkKeyChanges(t *testing.T, l *Ledger, addrs []basics.Address, kv basics.AppIndex) {
	latest := l.Latest()
	type keyCase struct {
		name   string
		obj    []byte
		keys   []string
		lookup func(VersionRange) (VersionPage, error)
	}
	cases := []keyCase{
		{"kv global", timetravel.AppParamsKey(kv), []string{"a", "n", "m", "c1", "c2", "t", "z", "", "nope"},
			func(r VersionRange) (VersionPage, error) { return l.AppVersions(kv, r) }},
		{"kv local", timetravel.LocalKey(addrs[1], kv), []string{"x", "y", "z", "nope"},
			func(r VersionRange) (VersionPage, error) { return l.AccountAppVersions(addrs[1], kv, r) }},
		{"genesis global", timetravel.AppParamsKey(genesisApp), []string{"g1", "g2", "nope"},
			func(r VersionRange) (VersionPage, error) { return l.AppVersions(genesisApp, r) }},
		{"genesis local", timetravel.LocalKey(addrs[6], genesisApp), []string{"l1", "nope"},
			func(r VersionRange) (VersionPage, error) { return l.AccountAppVersions(addrs[6], genesisApp, r) }},
	}
	for _, c := range cases {
		for _, key := range c.keys {
			p, err := c.lookup(VersionRange{Max: latest, Limit: 1000, Key: []byte(key)})
			require.NoError(t, err)
			require.False(t, p.More)
			checkChangeLookups(t, latest, keyChanges(p.Versions), func(from, to basics.Round, budget uint64) (Change, error) {
				ch, err := l.StateKeyChange(c.obj, []byte(key), from, to, budget, nil)
				if budget > 0 && ch.Status == ChangeMore {
					require.GreaterOrEqual(t, ch.Bytes, budget)
				}
				ch.Bytes = 0
				return ch, err
			}, c.name+" "+key)
		}
		p, err := l.objectVersions(c.obj, VersionRange{Max: latest, Limit: 1000})
		require.NoError(t, err)
		require.NotEmpty(t, p.Versions)
		checkChangeLookups(t, latest, objectChanges(p.Versions, func(v Version) bool {
			if len(v.Apps) > 0 {
				return v.Apps[0].Deleted
			}
			return v.LocalStates[0].Deleted
		}), func(from, to basics.Round, _ uint64) (Change, error) {
			return l.ObjectChange(c.obj, from, to, nil)
		}, c.name+" object")
	}

	_, err := l.StateKeyChange(timetravel.AccountKey(addrs[1]), []byte("a"), 0, latest, 0, nil)
	require.Error(t, err, "a state key applies to app params or a local state")
}

// checkBoxPrefixChanges checks BoxPrefixChange, and ObjectChange of boxes, against the versions
// lookups checkBoxPrefixVersions checks.
func checkBoxPrefixChanges(t *testing.T, l *Ledger, app basics.AppIndex) {
	latest := l.Latest()
	all, err := l.AppBoxVersions(app, VersionRange{Max: latest, Limit: 1000})
	require.NoError(t, err)
	require.False(t, all.More)
	exists := map[string]bool{}
	var chs []ttChange
	for _, v := range all.Versions {
		for _, b := range v.Boxes {
			chs = append(chs, ttChange{rnd: v.Round, status: changeStatus(!b.Deleted, exists[string(b.Name)]), name: b.Name})
			exists[string(b.Name)] = !b.Deleted
		}
	}
	for _, pfx := range []string{"", "a", "a\x00", "a\x00b", "ab", "c", "c1", "s", "t", "zz"} {
		var under []ttChange
		for _, c := range chs {
			if string(c.name[:min(len(c.name), len(pfx))]) == pfx {
				under = append(under, c)
			}
		}
		checkChangeLookups(t, latest, under, func(from, to basics.Round, budget uint64) (Change, error) {
			ch, err := l.BoxPrefixChange(app, []byte(pfx), from, to, budget, nil)
			ch.Bytes = 0
			return ch, err
		}, "prefix "+pfx)
	}
	// a box on its own, by name
	for name := range exists {
		var own []ttChange
		for _, c := range chs {
			if string(c.name) == name {
				own = append(own, ttChange{rnd: c.rnd, status: c.status})
			}
		}
		checkChangeLookups(t, latest, own, func(from, to basics.Round, _ uint64) (Change, error) {
			return l.ObjectChange(timetravel.BoxKey(uint64(app), []byte(name)), from, to, nil)
		}, "box "+name)
	}
}
