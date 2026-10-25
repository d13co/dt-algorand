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
	"context"
	"encoding/binary"
	"fmt"
	"math/rand"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"

	"github.com/algorand/go-deadlock"

	"github.com/algorand/go-algorand/test/partitiontest"
)

// commitRounds are the changes of a few hundred rounds over a small set of
// objects, boxes among them: values that repeat (no-ops), deletions,
// objects created and deleted in one round (a lone tombstone), and the
// expired stake of some rounds.
type commitRound struct {
	changes []Change
	expired *uint64
}

func commitRounds(rounds int) []commitRound {
	rng := rand.New(rand.NewSource(7))
	out := make([]commitRound, rounds+1)
	for r := 1; r <= rounds; r++ {
		n := 1 + rng.Intn(12)
		seen := map[string]bool{}
		var cr commitRound
		for range n {
			var key []byte
			switch rng.Intn(3) {
			case 0:
				key = KvKey(fmt.Sprintf("k%03d", rng.Intn(60)))
			case 1:
				key = boxObject(uint64(1+rng.Intn(3)), []byte(fmt.Sprintf("b%02d", rng.Intn(20))))
			default:
				key = KvKey(fmt.Sprintf("totals%d", rng.Intn(2))) // rewritten often, mostly unchanged
			}
			if seen[string(key)] {
				continue
			}
			seen[string(key)] = true
			ch := Change{Key: key, Value: []byte{byte(rng.Intn(4))}}
			switch rng.Intn(8) {
			case 0:
				ch = Change{Key: key, Deleted: true}
			case 1:
				ch.Value = bytes.Repeat([]byte{byte(r)}, 1+rng.Intn(300))
			}
			cr.changes = append(cr.changes, ch)
		}
		if r%3 == 0 {
			e := uint64(rng.Intn(100)) // 68 encodes as 'D', which is not a tombstone here
			cr.expired = &e
		}
		out[r] = cr
	}
	return out
}

func hdrOf(r uint64) []byte { return []byte(fmt.Sprintf("header of round %d", r)) }

// buildCommitStore writes rounds 0..upTo of rs to a new store.
func buildCommitStore(t *testing.T, rs []commitRound, upTo int, o Options, flushEvery int) *Store {
	st, err := Open(t.TempDir(), o)
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })
	require.NoError(t, st.AddRound(0, nil, hdrOf(0), nil))
	for r := 1; r <= upTo; r++ {
		require.NoError(t, st.AddRound(uint64(r), rs[r].changes, hdrOf(uint64(r)), rs[r].expired))
		if flushEvery > 0 && r%flushEvery == 0 {
			require.NoError(t, st.DB().Flush())
		}
	}
	return st
}

// randomSplits splits ranges at random keys of the store, so that ranges
// start inside objects, and at boundaries of every kind.
func randomSplits(st *Store, seed int64, n int) splitFunc {
	return func(_ *pebble.Snapshot, lo, hi []byte, _ int) ([][]byte, error) {
		rng := rand.New(rand.NewSource(seed))
		it, err := st.db.NewIter(&pebble.IterOptions{LowerBound: lo, UpperBound: hi})
		if err != nil {
			return nil, err
		}
		defer it.Close()
		var keys [][]byte
		for valid := it.First(); valid; valid = it.Next() {
			keys = append(keys, slices.Clone(it.Key()))
		}
		var out [][]byte
		for range n {
			if len(keys) > 0 {
				out = append(out, keys[rng.Intn(len(keys))])
			}
		}
		return out, it.Error()
	}
}

func commitOf(t *testing.T, st *Store, upTo uint64, workers int, splits splitFunc) Commitment {
	if splits == nil {
		splits = st.keySplits
	}
	c, err := st.commitment(context.Background(), upTo, CommitmentOptions{Workers: workers}, splits)
	require.NoError(t, err)
	return c
}

// TestCommitmentLayoutIndependent checks that stores with the same content
// have the same commitment, whatever the dedup cache, flushes, RemoveNoops,
// compaction, later rounds and the split among workers.
func TestCommitmentLayoutIndependent(t *testing.T) {
	partitiontest.PartitionTest(t)
	// chunks of a few objects, so that ranges end and start inside chunks
	defer func(f func(*[32]byte) bool) { commitBoundary = f }(commitBoundary)
	commitBoundary = func(d *[32]byte) bool { return d[0]%4 == 0 }

	rs := commitRounds(400)
	const upTo = 300
	base := buildCommitStore(t, rs, upTo, Options{}, 0)
	want := commitOf(t, base, upTo, 1, func(*pebble.Snapshot, []byte, []byte, int) ([][]byte, error) { return nil, nil })
	require.EqualValues(t, upTo, want.Round)
	require.EqualValues(t, upTo+1, want.Headers)
	require.EqualValues(t, upTo/3, want.Expired)
	require.Positive(t, want.Objects)
	require.Greater(t, want.Versions, want.Objects)

	noDedup := buildCommitStore(t, rs, upTo, Options{DedupEntries: -1}, 37)
	tinyDedup := buildCommitStore(t, rs, upTo, Options{DedupEntries: 2}, 0)
	longer := buildCommitStore(t, rs, 400, Options{DedupEntries: -1}, 50)
	for name, st := range map[string]*Store{"base": base, "no dedup": noDedup, "tiny dedup": tinyDedup, "more rounds": longer} {
		for _, workers := range []int{1, 3, 8} {
			for seed := range int64(4) {
				got := commitOf(t, st, upTo, workers, randomSplits(st, seed, int(seed)*7))
				require.Equal(t, want, got, "%s, %d workers, split %d", name, workers, seed)
			}
		}
		require.Equal(t, want, commitOf(t, st, upTo, 0, st.keySplits), name)
		func() {
			defer func(m uint64) { minSplitBytes = m }(minSplitBytes)
			minSplitBytes = 0 // split as finely as it can
			parts, err := st.keySplits(st.db.NewSnapshot(), []byte{pfxCheckpoint}, []byte{pfxCheckpoint + 1}, 40)
			require.NoError(t, err)
			require.Len(t, parts, 39, name)
			require.Equal(t, want, commitOf(t, st, upTo, 5, st.keySplits), name)
		}()
	}

	// the no-op records are there (otherwise the test proves little), and
	// removing them changes nothing
	removed, err := noDedup.RemoveNoops(context.Background(), nil)
	require.NoError(t, err)
	require.Positive(t, removed)
	require.NoError(t, noDedup.CompactCheckpoints(context.Background()))
	require.Equal(t, want, commitOf(t, noDedup, upTo, 4, randomSplits(noDedup, 9, 20)))

	// nor does the box index or the meta records
	_, err = base.IndexBoxes(context.Background(), nil)
	require.NoError(t, err)
	require.NoError(t, base.DB().Set([]byte("mz"), []byte("local"), pebble.Sync))
	require.Equal(t, want, commitOf(t, base, upTo, 2, base.keySplits))

	// an earlier round is another commitment
	require.NotEqual(t, want.Digest, commitOf(t, base, upTo-1, 2, base.keySplits).Digest)
}

// TestCommitmentDetectsChanges checks that each kind of change to the content
// changes the commitment.
func TestCommitmentDetectsChanges(t *testing.T) {
	partitiontest.PartitionTest(t)
	rs := commitRounds(100)
	const upTo = 100
	want := commitOf(t, buildCommitStore(t, rs, upTo, Options{}, 0), upTo, 2, nil)

	obj := KvKey("k007")
	for name, edit := range map[string]func(*pebble.Batch){
		"value":        func(b *pebble.Batch) { b.Set(verKey(pfxCheckpoint, obj, 50), []byte{recFull, 99}, nil) },
		"extra object": func(b *pebble.Batch) { b.Set(verKey(pfxCheckpoint, KvKey("planted"), 50), []byte{recFull, 1}, nil) },
		"header": func(b *pebble.Batch) {
			b.Set(binary.BigEndian.AppendUint64([]byte{pfxHeader}, 40), []byte("forged"), nil)
		},
		"expired stake":  func(b *pebble.Batch) { b.Set(binary.BigEndian.AppendUint64([]byte{pfxExpired}, 40), []byte{1}, nil) },
		"missing header": func(b *pebble.Batch) { b.Delete(binary.BigEndian.AppendUint64([]byte{pfxHeader}, 40), nil) },
		"missing version": func(b *pebble.Batch) {
			b.DeleteRange(append([]byte{pfxCheckpoint}, obj...), prefixEnd(append([]byte{pfxCheckpoint}, obj...)), nil)
		},
	} {
		t.Run(name, func(t *testing.T) {
			st := buildCommitStore(t, rs, upTo, Options{}, 0)
			b := st.DB().NewBatch()
			edit(b)
			require.NoError(t, b.Commit(pebble.Sync))
			got := commitOf(t, st, upTo, 2, nil)
			require.NotEqual(t, want.Digest, got.Digest)
		})
	}
}

func TestMidKey(t *testing.T) {
	partitiontest.PartitionTest(t)
	rng := rand.New(rand.NewSource(3))
	for range 10000 {
		a := make([]byte, rng.Intn(12))
		rng.Read(a)
		b := make([]byte, rng.Intn(12))
		rng.Read(b)
		if rng.Intn(2) == 0 && len(a) > 2 {
			b = append(slices.Clone(a[:rng.Intn(len(a))]), b...)
		}
		if bytes.Compare(a, b) >= 0 {
			a, b = b, a
		}
		if m := midKey(a, b); m != nil {
			require.Positive(t, bytes.Compare(m, a), "%x %x %x", a, b, m)
			require.LessOrEqual(t, bytes.Compare(m, b), 0, "%x %x %x", a, b, m)
		}
	}
	require.Nil(t, midKey([]byte{1, 2}, []byte{1, 2, 0}))
	require.Equal(t, []byte{'c', 0, 0, 0, 0, 0, 0, 0, 5, 0, 0, 0, 0, 0, 0, 0}, midKey([]byte{'c', 0, 0, 0, 0, 0, 0, 0, 1}, []byte{'c', 0, 0, 0, 0, 0, 0, 0, 9}))
}

func TestCommitmentRefuses(t *testing.T) {
	partitiontest.PartitionTest(t)
	rs := commitRounds(20)
	st := buildCommitStore(t, rs, 20, Options{}, 0)
	_, err := st.Commitment(context.Background(), 21, CommitmentOptions{Workers: 1})
	require.ErrorContains(t, err, "last round is 20")

	patched := buildCommitStore(t, rs, 20, Options{CheckpointInterval: 4}, 0)
	_, err = patched.Commitment(context.Background(), 20, CommitmentOptions{Workers: 1})
	require.ErrorContains(t, err, "N = 1")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = st.Commitment(ctx, 20, CommitmentOptions{Workers: 2})
	require.ErrorIs(t, err, context.Canceled)

	// Close stops a commitment rather than wait for it
	closed := make(chan error, 1)
	_, err = st.commitment(context.Background(), 20, CommitmentOptions{Workers: 2}, func(*pebble.Snapshot, []byte, []byte, int) ([][]byte, error) {
		go func() { closed <- st.Close() }()
		<-st.closing
		return nil, nil
	})
	require.ErrorIs(t, err, context.Canceled)
	require.NoError(t, <-closed)
}

// TestCommitmentStreams checks that content without chunk boundaries, as an
// on-chain user could make by grinding box values, gives the same commitment
// however it is split, with workers handing over their digests in batches.
func TestCommitmentStreams(t *testing.T) {
	partitiontest.PartitionTest(t)
	rs := commitRounds(300)
	st := buildCommitStore(t, rs, 300, Options{}, 0)
	for _, boundary := range []func(*[32]byte) bool{
		func(d *[32]byte) bool { return false },        // no chunk ends
		func(d *[32]byte) bool { return d[0]%16 == 0 }, // some do
	} {
		func() {
			defer func(f func(*[32]byte) bool, b int) { commitBoundary, commitBuffer = f, b }(commitBoundary, commitBuffer)
			commitBoundary = boundary
			want := commitOf(t, st, 300, 1, func(*pebble.Snapshot, []byte, []byte, int) ([][]byte, error) { return nil, nil })
			for _, buffer := range []int{1, 3, 1 << 20} {
				commitBuffer = buffer
				for seed := range int64(5) {
					require.Equal(t, want, commitOf(t, st, 300, 4, randomSplits(st, seed, 30)), "buffer %d, split %d", buffer, seed)
				}
			}
		}()
	}
}

// TestCommitmentStrict checks that a strict commitment refuses what reads
// could see beyond the content it covers, and that a plain one refuses keys
// that would make it depend on the split.
func TestCommitmentStrict(t *testing.T) {
	partitiontest.PartitionTest(t)
	rs := commitRounds(100)
	strict := CommitmentOptions{Workers: 2, Strict: true}
	clean := buildCommitStore(t, rs, 100, Options{}, 0)
	want, err := clean.Commitment(context.Background(), 100, strict)
	require.NoError(t, err)

	obj := KvKey("k007")
	var lastVer uint64
	vit, err := clean.db.NewIter(&pebble.IterOptions{LowerBound: append([]byte{pfxCheckpoint}, obj...), UpperBound: prefixEnd(append([]byte{pfxCheckpoint}, obj...))})
	require.NoError(t, err)
	for valid := vit.Last(); valid; valid = false {
		k := vit.Key()
		lastVer = binary.BigEndian.Uint64(k[len(k)-8:])
	}
	require.NoError(t, vit.Close())
	require.Positive(t, lastVer)

	for name, tc := range map[string]struct {
		edit   func(*pebble.Batch)
		upTo   uint64
		plain  string // what a plain commitment says, "" if it passes
		strict string
	}{
		// a lone tombstone whose object key extends obj's past its last
		// version: Get(obj) would find it and read obj as deleted
		"phantom tombstone": {edit: func(b *pebble.Batch) {
			phantom := append(binary.BigEndian.AppendUint64(slices.Clone(obj), lastVer), 'z')
			b.Set(verKey(pfxCheckpoint, phantom, 1), []byte{recDeleted}, nil)
		}, upTo: 100, plain: "follows", strict: "not an object key"},
		"record after the round": {edit: func(b *pebble.Batch) {
			b.Set(verKey(pfxCheckpoint, obj, 105), []byte{recFull, 0xee}, nil)
		}, upTo: 100, strict: "after round 100"},
		"header after the round": {edit: func(b *pebble.Batch) {}, upTo: 90, strict: "not of its history up to round 90"},
		"patch record": {edit: func(b *pebble.Batch) {
			b.Set(verKey(pfxPatch, KvKey("phantom"), 5), []byte{recFull, 1}, nil)
		}, upTo: 100, strict: "not of its history"},
		"foreign key": {edit: func(b *pebble.Batch) { b.Set([]byte("zzz"), nil, nil) }, upTo: 100, strict: "not of its history"},
		"meta record": {edit: func(b *pebble.Batch) { b.Set([]byte("mq"), nil, nil) }, upTo: 100, strict: "unknown meta record"},
	} {
		t.Run(name, func(t *testing.T) {
			st := buildCommitStore(t, rs, 100, Options{}, 0)
			b := st.db.NewBatch()
			tc.edit(b)
			require.NoError(t, b.Commit(pebble.Sync))
			_, err := st.Commitment(context.Background(), tc.upTo, CommitmentOptions{Workers: 2})
			if tc.plain == "" {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, ErrCorrupt)
				require.ErrorContains(t, err, tc.plain)
			}
			_, err = st.Commitment(context.Background(), tc.upTo, strict)
			require.ErrorIs(t, err, ErrCorrupt)
			require.ErrorContains(t, err, tc.strict)
		})
	}

	// the box index and the store's own meta records are not refused: the
	// installer rebuilds the box index
	st := buildCommitStore(t, rs, 100, Options{}, 0)
	require.NoError(t, st.db.DeleteRange([]byte{pfxBoxIndex}, []byte{pfxBoxIndex + 1}, pebble.Sync))
	require.NoError(t, st.SetInstalled("history-100.json", 100))
	got, err := st.Commitment(context.Background(), 100, strict)
	require.NoError(t, err)
	require.Equal(t, want, got)
	name, ok, err := st.Installed()
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "history-100.json", name)

	// NonCanonical names what RemoveNoops removes: deleting it changes
	// nothing, and leaves RemoveNoops nothing to do
	noDedup := buildCommitStore(t, rs, 100, Options{DedupEntries: -1}, 0)
	var keys [][]byte
	var mu deadlock.Mutex
	o := strict
	o.NonCanonical = func(k []byte) { mu.Lock(); keys = append(keys, k); mu.Unlock() }
	got, err = noDedup.Commitment(context.Background(), 100, o)
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.NotEmpty(t, keys)
	b := noDedup.db.NewBatch()
	for _, k := range keys {
		require.NoError(t, b.Delete(k, nil))
	}
	require.NoError(t, b.Commit(pebble.Sync))
	removed, err := noDedup.RemoveNoops(context.Background(), nil)
	require.NoError(t, err)
	require.Zero(t, removed)
	got, err = noDedup.Commitment(context.Background(), 100, strict)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

// TestCommitmentOfStore computes the commitment of the store at
// TT_COMMIT_STORE (opened read-only), up to TT_COMMIT_ROUND (default: its last
// round), and logs the time it took.
func TestCommitmentOfStore(t *testing.T) {
	partitiontest.PartitionTest(t)
	dir := os.Getenv("TT_COMMIT_STORE")
	if dir == "" {
		t.Skip("set TT_COMMIT_STORE")
	}
	st, err := Open(dir, Options{ReadOnly: true})
	require.NoError(t, err)
	defer st.Close()
	upTo, _ := st.Last()
	if r := os.Getenv("TT_COMMIT_ROUND"); r != "" {
		_, err := fmt.Sscan(r, &upTo)
		require.NoError(t, err)
	}
	workers := 0
	if w := os.Getenv("TT_COMMIT_WORKERS"); w != "" {
		_, err := fmt.Sscan(w, &workers)
		require.NoError(t, err)
	}
	start := time.Now()
	var read uint64
	c, err := st.Commitment(context.Background(), upTo, CommitmentOptions{Workers: workers, Progress: func(b uint64) { read = max(read, b) }})
	require.NoError(t, err)
	el := time.Since(start)
	t.Logf("commitment %x of rounds %d..%d: %d objects, %d versions, %d headers, %d expired; %.2f GB read in %v (%.0f MB/s)",
		c.Digest, c.First, c.Round, c.Objects, c.Versions, c.Headers, c.Expired, float64(read)/1e9, el.Round(time.Millisecond), float64(read)/1e6/el.Seconds())
}
