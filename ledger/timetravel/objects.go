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
	"crypto/sha256"
	"encoding/binary"
	"slices"

	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	"github.com/algorand/go-algorand/ledger/store/trackerdb"
	"github.com/algorand/go-algorand/protocol"
)

// Object kinds, the first byte of an object key.
const (
	KindAccount     = 'a' // addr          -> trackerdb.BaseAccountData
	KindHolding     = 'h' // addr|asset    -> basics.AssetHolding
	KindLocal       = 'l' // addr|app      -> basics.AppLocalState
	KindAssetParams = 'A' // asset         -> creator|basics.AssetParams
	KindAppParams   = 'P' // app           -> creator|basics.AppParams
	KindKv          = 'k' // escaped key   -> raw value
	KindTotals      = 'T' //               -> ledgercore.AccountTotals
	KindCreated     = 'C' // creator|type|id -> empty (creator index)
)

// TotalsKey is the object key of the account totals.
var TotalsKey = []byte{KindTotals}

// AccountKey is the object key of an account's base data.
func AccountKey(addr basics.Address) []byte { return append([]byte{KindAccount}, addr[:]...) }

// HoldingKey is the object key of an asset holding.
func HoldingKey(addr basics.Address, aidx basics.AssetIndex) []byte {
	return binary.BigEndian.AppendUint64(append([]byte{KindHolding}, addr[:]...), uint64(aidx))
}

// LocalKey is the object key of an app local state.
func LocalKey(addr basics.Address, aidx basics.AppIndex) []byte {
	return binary.BigEndian.AppendUint64(append([]byte{KindLocal}, addr[:]...), uint64(aidx))
}

// AssetParamsKey is the object key of asset params.
func AssetParamsKey(aidx basics.AssetIndex) []byte {
	return binary.BigEndian.AppendUint64([]byte{KindAssetParams}, uint64(aidx))
}

// AppParamsKey is the object key of app params.
func AppParamsKey(aidx basics.AppIndex) []byte {
	return binary.BigEndian.AppendUint64([]byte{KindAppParams}, uint64(aidx))
}

// CreatedKey is the object key of the creator index entry of an asset or app.
func CreatedKey(creator basics.Address, ctype basics.CreatableType, cidx basics.CreatableIndex) []byte {
	return binary.BigEndian.AppendUint64(CreatedPrefix(creator, ctype), uint64(cidx))
}

// CreatedPrefix is the object key prefix of the assets or apps created by creator.
func CreatedPrefix(creator basics.Address, ctype basics.CreatableType) []byte {
	return append(append([]byte{KindCreated}, creator[:]...), byte(ctype))
}

// KvKey is the object key of a kv (box) entry. Keys are escaped (0x00 ->
// 0x00 0xff) and terminated by 0x00 0x00 so that variable-length keys keep
// their order and no key's versions interleave with another's.
func KvKey(key string) []byte { return append(KvPrefix(key), 0, 0) }

// KvPrefix is the object key prefix of every kv key starting with prefix.
func KvPrefix(prefix string) []byte {
	out := make([]byte, 1, len(prefix)+8)
	out[0] = KindKv
	for i := 0; i < len(prefix); i++ {
		out = append(out, prefix[i])
		if prefix[i] == 0 {
			out = append(out, 0xff)
		}
	}
	return out
}

// ValidObjectKey reports whether obj is an object key of one of the kinds
// above, as the functions above make them. No valid key starts with another:
// those of a kind have one length, or for kv keys end at their terminator.
// A store holds no other keys, and a read of an object whose key another
// starts with could take the other's records for its own.
func ValidObjectKey(obj []byte) bool {
	if len(obj) == 0 {
		return false
	}
	switch obj[0] {
	case KindTotals:
		return len(obj) == 1
	case KindAccount:
		return len(obj) == 1+32
	case KindHolding, KindLocal:
		return len(obj) == 1+32+8
	case KindAssetParams, KindAppParams:
		return len(obj) == 1+8
	case KindCreated:
		return len(obj) == 1+32+1+8
	case KindKv:
		// escaped bytes, then 0x00 0x00 and nothing after
		for i := 1; i < len(obj); i++ {
			if obj[i] != 0 {
				continue
			}
			if i+1 >= len(obj) {
				return false
			}
			switch obj[i+1] {
			case 0xff:
				i++
			case 0:
				return i+2 == len(obj)
			default:
				return false
			}
		}
		return false
	}
	return false
}

// KvKeyString reverses KvKey.
func KvKeyString(obj []byte) string {
	b := make([]byte, 0, len(obj))
	for i := 1; i < len(obj)-2; i++ {
		b = append(b, obj[i])
		if obj[i] == 0 {
			i++
		}
	}
	return string(b)
}

// boxKeyPrefix starts the kv key of every box: "bx:", then the app id (8
// bytes, big endian), then the name.
const boxKeyPrefix = "bx:"

// kvBoxPrefix is the object key prefix of every box.
var kvBoxPrefix = KvPrefix(boxKeyPrefix)

// BoxKvPrefix is the object key prefix of the boxes of app.
func BoxKvPrefix(app uint64) []byte {
	return KvPrefix(string(binary.BigEndian.AppendUint64([]byte(boxKeyPrefix), app)))
}

// BoxOf returns the app and name of a box's object key; ok is false if obj
// is not a box's.
func BoxOf(obj []byte) (app uint64, name []byte, ok bool) {
	if !bytes.HasPrefix(obj, kvBoxPrefix) {
		return 0, nil, false
	}
	k := KvKeyString(obj)
	if len(k) < len(boxKeyPrefix)+8 {
		return 0, nil, false
	}
	return binary.BigEndian.Uint64([]byte(k[len(boxKeyPrefix):])), []byte(k[len(boxKeyPrefix)+8:]), true
}

// boxObject is the object key of box name of app.
func boxObject(app uint64, name []byte) []byte {
	return KvKey(string(append(binary.BigEndian.AppendUint64([]byte(boxKeyPrefix), app), name...)))
}

// Changes extracts the new object versions of a state delta, including totals.
func Changes(sd *ledgercore.StateDelta) []Change {
	out := make([]Change, 0, len(sd.Accts.Accts)+len(sd.Accts.AssetResources)+len(sd.Accts.AppResources)+len(sd.KvMods)+1)
	for i := range sd.Accts.Accts {
		br := &sd.Accts.Accts[i]
		var ba trackerdb.BaseAccountData
		ba.SetCoreAccountData(&br.AccountData)
		out = append(out, Change{Key: AccountKey(br.Addr), Value: protocol.Encode(&ba), Deleted: ba.IsEmpty()})
	}
	for _, r := range sd.Accts.AssetResources {
		if r.Holding.Deleted {
			out = append(out, Change{Key: HoldingKey(r.Addr, r.Aidx), Deleted: true})
		} else if r.Holding.Holding != nil {
			out = append(out, Change{Key: HoldingKey(r.Addr, r.Aidx), Value: protocol.Encode(r.Holding.Holding)})
		}
		if r.Params.Deleted {
			out = append(out, Change{Key: AssetParamsKey(r.Aidx), Deleted: true})
		} else if r.Params.Params != nil {
			out = append(out, Change{Key: AssetParamsKey(r.Aidx), Value: append(r.Addr[:], protocol.Encode(r.Params.Params)...)})
		}
	}
	for _, r := range sd.Accts.AppResources {
		if r.State.Deleted {
			out = append(out, Change{Key: LocalKey(r.Addr, r.Aidx), Deleted: true})
		} else if r.State.LocalState != nil {
			out = append(out, Change{Key: LocalKey(r.Addr, r.Aidx), Value: protocol.Encode(r.State.LocalState)})
		}
		if r.Params.Deleted {
			out = append(out, Change{Key: AppParamsKey(r.Aidx), Deleted: true})
		} else if r.Params.Params != nil {
			out = append(out, Change{Key: AppParamsKey(r.Aidx), Value: append(r.Addr[:], protocol.Encode(r.Params.Params)...)})
		}
	}
	for cidx, mc := range sd.Creatables {
		out = append(out, Change{Key: CreatedKey(mc.Creator, mc.Ctype, cidx), Value: []byte{}, Deleted: !mc.Created})
	}
	for k, v := range sd.KvMods {
		out = append(out, Change{Key: KvKey(k), Value: v.Data, Deleted: v.Data == nil})
	}
	out = append(out, Change{Key: TotalsKey, Value: protocol.Encode(&sd.Totals)})
	return out
}

// GenesisChanges returns the objects of the genesis allocation.
func GenesisChanges(accts map[basics.Address]basics.AccountData, totals ledgercore.AccountTotals) []Change {
	out := []Change{{Key: TotalsKey, Value: protocol.Encode(&totals)}}
	for addr, ad := range accts {
		var ba trackerdb.BaseAccountData
		ba.SetAccountData(&ad)
		out = append(out, Change{Key: AccountKey(addr), Value: protocol.Encode(&ba)})
		for aidx, p := range ad.AssetParams {
			out = append(out, Change{Key: AssetParamsKey(aidx), Value: append(addr[:], protocol.Encode(&p)...)},
				Change{Key: CreatedKey(addr, basics.AssetCreatable, basics.CreatableIndex(aidx)), Value: []byte{}})
		}
		for aidx, h := range ad.Assets {
			out = append(out, Change{Key: HoldingKey(addr, aidx), Value: protocol.Encode(&h)})
		}
		for aidx, p := range ad.AppParams {
			out = append(out, Change{Key: AppParamsKey(aidx), Value: append(addr[:], protocol.Encode(&p)...)},
				Change{Key: CreatedKey(addr, basics.AppCreatable, basics.CreatableIndex(aidx)), Value: []byte{}})
		}
		for aidx, ls := range ad.AppLocalStates {
			out = append(out, Change{Key: LocalKey(addr, aidx), Value: protocol.Encode(&ls)})
		}
	}
	return out
}

// DigestLen is the length of a ChangesDigest.
const DigestLen = 8

// ChangesDigest is a short digest of a round's changes, independent of their
// order. The store keeps one per round so that a re-evaluated round can be
// checked against the stored one as a whole.
func ChangesDigest(changes []Change) []byte {
	sorted := slices.Clone(changes)
	slices.SortStableFunc(sorted, func(a, b Change) int { return bytes.Compare(a.Key, b.Key) })
	h := sha256.New()
	var buf []byte
	for _, ch := range sorted {
		buf = binary.AppendUvarint(buf[:0], uint64(len(ch.Key)))
		buf = append(buf, ch.Key...)
		if ch.Deleted {
			buf = append(buf, 0)
		} else {
			buf = append(buf, 1)
			buf = binary.AppendUvarint(buf, uint64(len(ch.Value)))
			buf = append(buf, ch.Value...)
		}
		h.Write(buf)
	}
	return h.Sum(nil)[:DigestLen]
}
