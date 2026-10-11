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

// Package ttview renders REST values of a time travel node as text: flat
// key/value blocks, tables, and the changes between an object's versions.
// It works on JSON, so it serves the offline store dump (tools/ttdump) and
// goal alike.
package ttview

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"unicode"
	"unicode/utf8"

	"github.com/algorand/go-algorand/data/basics"
)

// AppCallBytes encodes b in goal's app call argument form: "str:" if it is
// printable UTF-8, "b64:" otherwise. apps.NewAppCallBytes decodes it back.
func AppCallBytes(b []byte) string {
	if printable(b) {
		return "str:" + string(b)
	}
	return "b64:" + base64.StdEncoding.EncodeToString(b)
}

func printable(b []byte) bool {
	if !utf8.Valid(b) {
		return false
	}
	for _, r := range string(b) {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// summarizeOver is the length above which FormatBytes summarizes a value.
const summarizeOver = 128

// FormatBytes renders a byte value for reading: AppCallBytes, except that
// 32 bytes that are not printable are shown as "addr:" and an address, and
// values longer than 128 bytes as their length and a hash prefix.
func FormatBytes(b []byte) string {
	switch {
	case printable(b) && len(b) <= summarizeOver:
		return "str:" + string(b)
	case len(b) == len(basics.Address{}):
		return "addr:" + basics.Address(b).String()
	case len(b) > summarizeOver:
		return summarize(b)
	}
	return AppCallBytes(b)
}

// formatBinary renders bytes that are neither text nor an address, such as
// keys, hashes and programs: as "b64:", or summarized when long.
func formatBinary(b []byte) string {
	if len(b) > summarizeOver {
		return summarize(b)
	}
	return "b64:" + base64.StdEncoding.EncodeToString(b)
}

func summarize(b []byte) string {
	sum := sha256.Sum256(b)
	return fmt.Sprintf("<%d bytes, sha256 %s>", len(b), hex.EncodeToString(sum[:6]))
}
