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
	"encoding/binary"
	"errors"
)

// Patches are copy/insert byte deltas (git-delta style) over encoded objects:
//
//	uvarint(len(target)) op*
//	op = uvarint(n<<1)   n literal bytes      insert
//	   | uvarint(n<<1|1) uvarint(offset)      copy n bytes of base at offset
//
// They are schema-agnostic, so msgpack objects and raw box bytes share one codec.

const (
	matchWindow   = 8
	minCopy       = 12 // a copy op costs 2-5 bytes; shorter matches go out as literals
	maxPatchedLen = 1 << 26
)

var errBadPatch = errors.New("timetravel: malformed patch")

// Diff returns a patch that rebuilds target from base.
func Diff(base, target []byte) []byte {
	p := 0
	for p < len(base) && p < len(target) && base[p] == target[p] {
		p++
	}
	s := 0
	for s < len(base)-p && s < len(target)-p && base[len(base)-1-s] == target[len(target)-1-s] {
		s++
	}
	out := binary.AppendUvarint(make([]byte, 0, 16), uint64(len(target)))
	out = appendCopy(out, 0, p)
	out = diffMiddle(out, base, target[p:len(target)-s])
	return appendCopy(out, len(base)-s, s)
}

// diffMiddle encodes mid against matches anywhere in base.
func diffMiddle(out, base, mid []byte) []byte {
	if len(mid) < minCopy || len(base) < matchWindow {
		return appendInsert(out, mid)
	}
	step := 1
	if len(base) > 4096 {
		step = 4
	}
	idx := make(map[uint64]int32, len(base)/step)
	for i := 0; i+matchWindow <= len(base); i += step {
		h := binary.LittleEndian.Uint64(base[i:])
		if _, ok := idx[h]; !ok {
			idx[h] = int32(i)
		}
	}
	lit := 0
	for j := 0; j+matchWindow <= len(mid); {
		i, ok := idx[binary.LittleEndian.Uint64(mid[j:])]
		if !ok {
			j++
			continue
		}
		bi, bj := int(i), j
		for bi > 0 && bj > lit && base[bi-1] == mid[bj-1] {
			bi--
			bj--
		}
		n := j - bj + matchWindow
		for bi+n < len(base) && bj+n < len(mid) && base[bi+n] == mid[bj+n] {
			n++
		}
		if n < minCopy {
			j++
			continue
		}
		out = appendInsert(out, mid[lit:bj])
		out = appendCopy(out, bi, n)
		j = bj + n
		lit = j
	}
	return appendInsert(out, mid[lit:])
}

func appendInsert(out, b []byte) []byte {
	if len(b) == 0 {
		return out
	}
	out = binary.AppendUvarint(out, uint64(len(b))<<1)
	return append(out, b...)
}

func appendCopy(out []byte, off, n int) []byte {
	if n == 0 {
		return out
	}
	out = binary.AppendUvarint(out, uint64(n)<<1|1)
	return binary.AppendUvarint(out, uint64(off))
}

// Apply rebuilds the target that patch was produced for.
func Apply(base, patch []byte) ([]byte, error) {
	n, k := binary.Uvarint(patch)
	if k <= 0 || n > maxPatchedLen {
		return nil, errBadPatch
	}
	patch = patch[k:]
	out := make([]byte, 0, n)
	for len(patch) > 0 {
		op, k := binary.Uvarint(patch)
		if k <= 0 {
			return nil, errBadPatch
		}
		patch = patch[k:]
		l := op >> 1
		if op&1 == 0 {
			if l > uint64(len(patch)) {
				return nil, errBadPatch
			}
			out = append(out, patch[:l]...)
			patch = patch[l:]
			continue
		}
		off, k := binary.Uvarint(patch)
		if k <= 0 || off > uint64(len(base)) || l > uint64(len(base))-off {
			return nil, errBadPatch
		}
		patch = patch[k:]
		out = append(out, base[off:off+l]...)
	}
	if uint64(len(out)) != n {
		return nil, errBadPatch
	}
	return out, nil
}
