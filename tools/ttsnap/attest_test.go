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

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/crypto"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/data/transactions/logic"
	"github.com/algorand/go-algorand/ledger/timetravel"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// TestStatement checks the statement's encoding and that its signature is a
// deterministic Falcon-1024 signature of those bytes, which is what the AVM's
// falcon_verify checks (it calls FalconVerifier.VerifyBytes).
func TestStatement(t *testing.T) {
	partitiontest.PartitionTest(t)
	s := Statement{Round: 0x0102030405060708}
	for i := range 32 {
		s.GenesisHash[i], s.HeaderHash[i], s.Commitment[i] = 1, 2, 3
	}
	b := s.Bytes()
	require.Len(t, b, statementLen)
	require.Equal(t, 134, statementLen)
	require.Equal(t, statementPrefix, string(b[:30]))
	require.Equal(t, bytes.Repeat([]byte{1}, 32), b[30:62])
	require.Equal(t, uint64(0x0102030405060708), binary.BigEndian.Uint64(b[62:70]))
	require.Equal(t, bytes.Repeat([]byte{2}, 32), b[70:102])
	require.Equal(t, bytes.Repeat([]byte{3}, 32), b[102:134])

	_, signer := newKey(t)
	f1, err := signFile(signer, b)
	require.NoError(t, err)
	f2, err := signFile(signer, b)
	require.NoError(t, err)
	require.Equal(t, f1, f2, "det1024 signatures are deterministic")
	id, sig, err := parseSigFile(f1)
	require.NoError(t, err)
	require.Equal(t, keyID(signer.PublicKey), id)
	require.LessOrEqual(t, len(sig), crypto.FalconMaxSignatureSize)
	v := crypto.FalconVerifier{PublicKey: signer.PublicKey}
	require.NoError(t, v.VerifyBytes(b, sig))

	// a manifest signature never verifies as a statement's, nor the reverse
	keys := []Key{{Name: "a", Role: roleAttester, PK: signer.PublicKey}}
	mf, err := signFile(signer, manifestMessage(b))
	require.NoError(t, err)
	_, err = verifySigFile(mf, b, keys, roleAttester)
	require.ErrorContains(t, err, "does not verify")
	_, err = verifySigFile(f1, b, keys, roleAttester)
	require.NoError(t, err)
	_, err = verifySigFile(f1, b, keys, roleProducer)
	require.ErrorContains(t, err, "not a producer key")
}

// TestStatementOnChain checks a statement's signature file with the AVM's
// falcon_verify, as an on-chain registry would: a logic signature that
// approves (statement, signature) for the signer's public key.
func TestStatementOnChain(t *testing.T) {
	partitiontest.PartitionTest(t)
	_, signer := newKey(t)
	s := Statement{Round: 12345}
	s.Commitment[5] = 9
	f, err := signFile(signer, s.Bytes())
	require.NoError(t, err)
	_, sig, err := parseSigFile(f)
	require.NoError(t, err)

	ops, err := logic.AssembleStringWithVersion(fmt.Sprintf("arg 0; arg 1; byte 0x%s; falcon_verify", hex.EncodeToString(signer.PublicKey[:])), 12)
	require.NoError(t, err)
	proto := config.Consensus[protocol.ConsensusCurrentVersion]
	eval := func(stmt []byte) bool {
		txn := transactions.SignedTxn{Lsig: transactions.LogicSig{Logic: ops.Program, Args: [][]byte{stmt, sig}}}
		pass, err := logic.EvalSignature(0, logic.NewSigEvalParams([]transactions.SignedTxn{txn}, &proto, &logic.NoHeaderLedger{}))
		require.NoError(t, err)
		return pass
	}
	require.True(t, eval(s.Bytes()))
	s.Round++
	require.False(t, eval(s.Bytes()))
}

func TestReadKeys(t *testing.T) {
	partitiontest.PartitionTest(t)
	_, a := newKey(t)
	_, b := newKey(t)
	keys, err := readKeys(writeKeys(t, keyLine("a", roleProducer, a.PublicKey), "  # comment", keyLine("b", roleAttester, b.PublicKey)))
	require.NoError(t, err)
	require.Len(t, keys, 2)
	require.Equal(t, a.PublicKey, keys[0].PK)
	require.Equal(t, roleAttester, keys[1].Role)

	for name, line := range map[string]string{
		"role":   keyLine("a", "admin", a.PublicKey),
		"fields": "a producer",
		"key":    "a producer AAAA",
		"twice":  keyLine("a", roleProducer, a.PublicKey) + "\n" + keyLine("c", roleAttester, a.PublicKey),
	} {
		_, err := readKeys(writeKeys(t, line))
		require.Error(t, err, name)
	}
}

// attesterNet is a testNet whose producer data directory also serves as an
// attester's node, with keys for two attesters.
type attesterNet struct {
	*testNet
	dataDir string // the producer's data dir
	a1, a2  *crypto.FalconSigner
}

func newAttesterNet(t *testing.T) *attesterNet {
	n := &attesterNet{testNet: newTestNet(t)}
	n.dataDir = filepath.Dir(n.producer)
	_, n.a1 = newKey(t)
	_, n.a2 = newKey(t)
	n.keys = writeKeys(t, keyLine("producer1", roleProducer, n.signer.PublicKey),
		keyLine("att1", roleAttester, n.a1.PublicKey), keyLine("att2", roleAttester, n.a2.PublicKey))
	return n
}

func (n *attesterNet) attest(signer *crypto.FalconSigner, dataDir string) (string, error) {
	return attest(attestOpts{signer: signer, dataDir: dataDir, genesis: n.genesis, base: n.srv.URL, out: n.out, workers: 2, client: http.DefaultClient, log: io.Discard})
}

func TestFetchAttested(t *testing.T) {
	partitiontest.PartitionTest(t)
	n := newAttesterNet(t)
	m := n.publish()
	minAttest := func(k int) func(*fetchOpts) { return func(o *fetchOpts) { o.minAttest = k } }

	// the producer's own statement signature does not count
	_, err := n.fetch(n.consumer(), minAttest(1))
	require.ErrorContains(t, err, "0 attesters signed the statement")

	// the nodes ahead of the snapshot attest it
	n.addRounds(5)
	p, err := n.attest(n.a1, n.dataDir)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(n.out, filepath.FromSlash(statementSigPath(n.genesis.ID(), m.History.Last, keyID(n.a1.PublicKey)))), p)
	_, err = n.fetch(n.consumer(), minAttest(1))
	require.NoError(t, err)
	_, err = n.fetch(n.consumer(), minAttest(2))
	require.ErrorContains(t, err, "1 attesters signed the statement (att1)")

	// att2 signs another statement: it does not count
	s, err := m.statement()
	require.NoError(t, err)
	s.Commitment[0]++
	bad, err := signFile(n.a2, s.Bytes())
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(n.out, filepath.FromSlash(statementSigPath(n.genesis.ID(), m.History.Last, keyID(n.a2.PublicKey)))), bad, 0o644))
	var log bytes.Buffer
	_, err = n.fetch(n.consumer(), minAttest(2), func(o *fetchOpts) { o.progressLog = &log })
	require.ErrorContains(t, err, "1 attesters signed")
	require.Contains(t, log.String(), "attester att2: the signature by att2")

	_, err = n.attest(n.a2, n.dataDir)
	require.NoError(t, err)
	_, err = n.fetch(n.consumer(), minAttest(2))
	require.NoError(t, err)

	// the published statement is the one the signatures sign
	hexStmt, err := os.ReadFile(filepath.Join(n.out, filepath.FromSlash(statementDir(n.genesis.ID(), m.History.Last)), "statement.hex"))
	require.NoError(t, err)
	want, err := m.statement()
	require.NoError(t, err)
	require.Equal(t, hex.EncodeToString(want.Bytes()), strings.TrimSpace(string(hexStmt)))
}

func TestAttestRefuses(t *testing.T) {
	partitiontest.PartitionTest(t)
	n := newAttesterNet(t)
	n.publish()

	// a node whose history was installed from a snapshot
	consumer := n.consumer()
	_, err := n.fetch(consumer)
	require.NoError(t, err)
	_, err = n.attest(n.a1, consumer)
	require.ErrorContains(t, err, "installed from a snapshot")
	// the mark in the store goes with it, ttsnap.json or not
	require.NoError(t, os.Remove(filepath.Join(consumer, n.genesis.ID(), installedFile)))
	_, err = n.attest(n.a1, consumer)
	require.ErrorIs(t, err, errInstalled)

	// a node whose history differs
	st, err := timetravel.Open(filepath.Join(n.producer, historyDir), timetravel.Options{})
	require.NoError(t, err)
	require.NoError(t, st.DB().Set(append(append([]byte{'c'}, timetravel.KvKey("planted")...), binary.BigEndian.AppendUint64(nil, 3)...), []byte{'F', 1}, nil))
	require.NoError(t, st.Close())
	_, err = n.attest(n.a1, n.dataDir)
	require.ErrorContains(t, err, "differs from the snapshot's")
	_, err = os.Stat(filepath.Join(n.out, filepath.FromSlash(statementSigPath(n.genesis.ID(), basics.Round(n.rounds), keyID(n.a1.PublicKey)))))
	require.True(t, os.IsNotExist(err))

	// a round of its own: the node attests what its history says
	p, err := attest(attestOpts{signer: n.a1, dataDir: n.dataDir, genesis: n.genesis, round: 7, out: n.out, client: http.DefaultClient, log: io.Discard})
	require.NoError(t, err)
	require.FileExists(t, p)
}

// TestFetchCommitmentMismatch checks that a history whose content is not the
// manifest's commitment is not installed, though every file matches.
func TestFetchCommitmentMismatch(t *testing.T) {
	partitiontest.PartitionTest(t)
	n := newTestNet(t)
	n.publish()
	n.rewriteLatest(func(m *Manifest) {
		d := []byte(m.Commitment.Digest)
		d[0] ^= 1
		m.Commitment.Digest = string(d)
	})
	dataDir := n.consumer()
	_, err := n.fetch(dataDir)
	require.ErrorContains(t, err, "the downloaded history: its commitment is")
	_, err = os.Stat(n.installedHistory(dataDir))
	require.True(t, os.IsNotExist(err), "not installed")

	n.rewriteLatest(func(m *Manifest) { m.Commitment = nil })
	_, err = n.fetch(n.consumer())
	require.ErrorContains(t, err, "no commitment")
}

// TestGCStatements checks that gc removes the statements of the snapshots it
// removes, and only those.
func TestGCStatements(t *testing.T) {
	partitiontest.PartitionTest(t)
	n := newAttesterNet(t)
	first := n.publish()
	_, err := attest(attestOpts{signer: n.a1, dataDir: n.dataDir, genesis: n.genesis, round: 9, out: n.out, client: http.DefaultClient, log: io.Discard})
	require.NoError(t, err)
	n.addRounds(5)
	second := n.publish()
	_, _, err = gc(n.out, 1)
	require.NoError(t, err)
	dir := func(r basics.Round) string {
		return filepath.Join(n.out, filepath.FromSlash(statementDir(n.genesis.ID(), r)))
	}
	require.NoDirExists(t, dir(first.History.Last))
	require.DirExists(t, dir(second.History.Last))
	require.DirExists(t, dir(9), "a statement about a round without a snapshot stays")
}

func TestCatchpointLabel(t *testing.T) {
	partitiontest.PartitionTest(t)
	n := newTestNet(t)
	label := func(r uint64) string { return fmt.Sprintf("%d#%s", r, crypto.Digest{}.String()) }
	// the history ends at round 30; a catchpoint at 350 needs round 30
	require.NoError(t, checkLabel(label(350), 30))
	require.ErrorContains(t, checkLabel(label(351), 30), "needs round 31")

	_, _, err := prepare(n.producer, false, nil)
	require.NoError(t, err)
	m, err := publish(n.producer, n.out, publishOpts{signer: n.signer, catchpoint: label(351)}, nil)
	require.NoError(t, err)
	require.Empty(t, m.Catchpoint, "a catchpoint the history doesn't reach is left out")

	defer func(u string) { catchpointURL = u }(catchpointURL)
	catchpointURL = n.srv.URL + "/cp/%s"
	require.NoError(t, os.MkdirAll(filepath.Join(n.out, "cp"), 0o755))
	setLatest := func(l string) {
		require.NoError(t, os.WriteFile(filepath.Join(n.out, "cp", "ttsnaptest"), []byte(l+"\n"), 0o644))
	}
	ctx := context.Background()
	setLatest(label(340))
	got, err := chooseLabel(ctx, http.DefaultClient, n.genesis, nil, false)
	require.NoError(t, err)
	require.Equal(t, label(340), got, "no snapshot installed: the latest")

	snap := &Manifest{Info: Info{History: HistoryInfo{Last: 30}}, Catchpoint: label(345)}
	got, err = chooseLabel(ctx, http.DefaultClient, n.genesis, snap, false)
	require.NoError(t, err)
	require.Equal(t, label(340), got, "the latest, which the history reaches")

	setLatest(label(10351))
	_, err = chooseLabel(ctx, http.DefaultClient, n.genesis, snap, false)
	require.ErrorContains(t, err, "pass -snapshot-label", "the producer's label only when asked for")
	require.ErrorIs(t, err, errLatestPastHistory)
	hinted := withSnapshotLabelHint(err, "/var/lib/dt algorand")
	require.ErrorIs(t, hinted, errLatestPastHistory)
	require.ErrorContains(t, hinted, "\n  ttsnap catchup -d '/var/lib/dt algorand' -snapshot-label", "the refusal names the command that accepts the producer's label")
	other := errors.New("looking up the latest catchpoint")
	require.Equal(t, other, withSnapshotLabelHint(other, "/d"), "other errors get no hint")
	got, err = chooseLabel(ctx, http.DefaultClient, n.genesis, snap, true)
	require.NoError(t, err)
	require.Equal(t, label(345), got, "the snapshot's, when asked for and the latest is past the history")
	got, err = chooseLabel(ctx, http.DefaultClient, n.genesis, &Manifest{Info: snap.Info, Catchpoint: label(400)}, true)
	require.NoError(t, err)
	require.Equal(t, label(10351), got, "a recorded label the history doesn't reach is ignored (and the latest refused by the caller)")

	require.NoError(t, os.Remove(filepath.Join(n.out, "cp", "ttsnaptest")))
	_, err = chooseLabel(ctx, http.DefaultClient, n.genesis, snap, true)
	require.ErrorContains(t, err, "looking up the latest catchpoint", "no fallback when the latest can't be read")
}

// TestFetchCanonicalizes checks that what the commitment leaves out of a
// snapshot (no-op records, lone tombstones, the box index) does not survive
// the install: the installed store answers like any other with the
// commitment.
func TestFetchCanonicalizes(t *testing.T) {
	partitiontest.PartitionTest(t)
	n := newTestNet(t)
	st, err := timetravel.Open(filepath.Join(n.producer, historyDir), timetravel.Options{})
	require.NoError(t, err)
	db := st.DB()
	cKey := func(obj []byte, rnd uint64) []byte {
		return binary.BigEndian.AppendUint64(append([]byte{'c'}, obj...), rnd)
	}
	ghost, dup := timetravel.KvKey("ghost"), timetravel.KvKey("dup")
	require.NoError(t, db.Set(cKey(ghost, 5), []byte{'D'}, nil))      // a lone tombstone
	require.NoError(t, db.Set(cKey(dup, 3), []byte{'F', 1}, nil))     // a version
	require.NoError(t, db.Set(cKey(dup, 4), []byte{'F', 1}, nil))     // and a no-op
	require.NoError(t, db.DeleteRange([]byte{'b'}, []byte{'c'}, nil)) // no box index
	require.NoError(t, st.Close())
	m := n.publish()

	dataDir := n.consumer()
	_, err = n.fetch(dataDir)
	require.NoError(t, err)
	st, err = timetravel.Open(n.installedHistory(dataDir), timetravel.Options{ReadOnly: true})
	require.NoError(t, err)
	defer st.Close()
	for k, want := range map[string]bool{string(cKey(ghost, 5)): false, string(cKey(dup, 3)): true, string(cKey(dup, 4)): false} {
		_, closer, err := st.DB().Get([]byte(k))
		if err == nil {
			closer.Close()
		}
		require.Equal(t, want, err == nil, "%x", k)
	}
	it, err := st.DB().NewIter(&pebble.IterOptions{LowerBound: []byte{'b'}, UpperBound: []byte{'c'}})
	require.NoError(t, err)
	require.True(t, it.First(), "the box index is rebuilt")
	require.NoError(t, it.Close())
	name, ok, err := st.Installed()
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, manifestName(kindHistory, m.History.Last), name)
	c, err := st.Commitment(context.Background(), uint64(m.History.Last), timetravel.CommitmentOptions{Strict: true})
	require.NoError(t, err)
	require.Equal(t, m.Commitment.Digest, hex.EncodeToString(c.Digest[:]))
}

// TestFetchRefusesUncovered checks that a snapshot whose store holds records
// the commitment does not cover is not installed.
func TestFetchRefusesUncovered(t *testing.T) {
	partitiontest.PartitionTest(t)
	n := newTestNet(t)
	st, err := timetravel.Open(filepath.Join(n.producer, historyDir), timetravel.Options{})
	require.NoError(t, err)
	// a value for after the snapshot, which replayed rounds would not overwrite
	require.NoError(t, st.DB().Set(binary.BigEndian.AppendUint64(append([]byte{'c'}, timetravel.KvKey("later")...), n.rounds+3), []byte{'F', 7}, nil))
	require.NoError(t, st.Close())
	n.publish()
	dataDir := n.consumer()
	_, err = n.fetch(dataDir)
	require.ErrorContains(t, err, "is after round 30")
	_, err = os.Stat(n.installedHistory(dataDir))
	require.True(t, os.IsNotExist(err), "not installed")
}
