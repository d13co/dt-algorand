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

package ttview

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// Field is one leaf of a flattened value: its path and rendered value.
type Field struct {
	Path, Value string
}

// Options tune Flatten.
type Options struct {
	// Bytes names, in addition to the REST fields that are always bytes
	// (programs, keys, hashes), the fields whose strings are base64 bytes,
	// such as a box's "name" and "value".
	Bytes []string
	// Trim is removed from the start of every path, e.g. "params.".
	Trim string
}

// bytesFields are the REST fields whose strings are base64 bytes that are
// neither text nor addresses.
var bytesFields = map[string]bool{
	"approval-program":            true,
	"clear-state-program":         true,
	"metadata-hash":               true,
	"vote-participation-key":      true,
	"selection-participation-key": true,
	"state-proof-key":             true,
}

// idFields identify the elements of an array of objects, in order of
// preference: holdings and local states by asset or app, created assets and
// apps by index or id.
var idFields = []string{"asset-id", "app-id", "id", "index"}

// Flatten turns v, any value that encoding/json marshals (or JSON in a
// json.RawMessage), into its leaves in document order. Objects nest with
// ".", arrays with "[i]", except arrays keyed by an id field ("[12]") and
// TEAL key/value stores, whose entries are keyed by their key and hold the
// value itself ("global-state[str:counter]"). Byte strings are rendered with
// FormatBytes, nulls left out.
func Flatten(v any, o Options) ([]Field, error) {
	raw, ok := v.(json.RawMessage)
	if !ok {
		var err error
		if raw, err = json.Marshal(v); err != nil {
			return nil, err
		}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // uint64 amounts must not go through float64
	doc, err := decodeOrdered(dec)
	if err != nil {
		return nil, err
	}
	f := flattener{bytes: map[string]bool{}}
	for _, b := range o.Bytes {
		f.bytes[b] = true
	}
	f.walk("", "", doc, true)
	if o.Trim != "" {
		for i := range f.out {
			f.out[i].Path = strings.TrimPrefix(f.out[i].Path, o.Trim)
		}
	}
	return f.out, nil
}

// member is one field of a decoded object, which keeps its document order.
type member struct {
	key string
	val any
}

// decodeOrdered decodes the next JSON value: objects as []member, arrays as
// []any, numbers as json.Number.
func decodeOrdered(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return tok, nil
	}
	switch d {
	case '{':
		obj := []member{}
		for dec.More() {
			var k json.Token
			if k, err = dec.Token(); err != nil {
				return nil, err
			}
			var v any
			if v, err = decodeOrdered(dec); err != nil {
				return nil, err
			}
			obj = append(obj, member{k.(string), v})
		}
		_, err = dec.Token()
		return obj, err
	case '[':
		arr := []any{}
		for dec.More() {
			var v any
			if v, err = decodeOrdered(dec); err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
		_, err = dec.Token()
		return arr, err
	}
	return nil, fmt.Errorf("unexpected %v", d)
}

type flattener struct {
	bytes map[string]bool
	out   []Field
}

func join(path, seg string) string {
	if path == "" {
		return seg
	}
	return path + "." + seg
}

func field(obj []member, key string) (any, bool) {
	for _, m := range obj {
		if m.key == key {
			return m.val, true
		}
	}
	return nil, false
}

// walk flattens v, the value of field name at path.
func (f *flattener) walk(path, name string, v any, top bool) {
	switch v := v.(type) {
	case nil:
	case []member:
		for _, m := range v {
			// the round of the response, which a history prints on its own,
			// and the base64 copies of an asset's names
			if top && m.key == "round" || strings.HasSuffix(m.key, "-b64") {
				continue
			}
			f.walk(join(path, m.key), m.key, m.val, false)
		}
	case []any:
		for i, e := range v {
			if k, val, ok := tealEntry(e); ok {
				f.walk(path+"["+k+"]", "", val, false)
				continue
			}
			f.walk(fmt.Sprintf("%s[%s]", path, elementID(v, i)), "", e, false)
		}
	case string:
		if f.bytes[name] || bytesFields[name] {
			if b, err := base64.StdEncoding.DecodeString(v); err == nil {
				if f.bytes[name] {
					v = FormatBytes(b)
				} else {
					v = formatBinary(b)
				}
			}
		}
		f.out = append(f.out, Field{path, v})
	default:
		f.out = append(f.out, Field{path, fmt.Sprint(v)})
	}
}

// elementID is how element i of arr is named in a path: its id field if
// every element of arr has the same one, its index otherwise.
func elementID(arr []any, i int) string {
	for _, idf := range idFields {
		all := true
		for _, e := range arr {
			obj, ok := e.([]member)
			if !ok {
				all = false
				break
			}
			if _, ok := field(obj, idf); !ok {
				all = false
				break
			}
		}
		if all {
			id, _ := field(arr[i].([]member), idf)
			return fmt.Sprint(id)
		}
	}
	return fmt.Sprint(i)
}

// tealEntry recognizes an entry of a TEAL key/value store, {"key": base64,
// "value": {"type": 1 (bytes) or 2 (uint), "bytes": base64, "uint": n}}, and
// returns its rendered key and value.
func tealEntry(e any) (string, any, bool) {
	obj, ok := e.([]member)
	if !ok || len(obj) != 2 {
		return "", nil, false
	}
	k, _ := field(obj, "key")
	val, _ := field(obj, "value")
	key, ok1 := k.(string)
	tv, ok2 := val.([]member)
	if !ok1 || !ok2 {
		return "", nil, false
	}
	kb, err := base64.StdEncoding.DecodeString(key)
	if err != nil {
		return "", nil, false
	}
	typ, _ := field(tv, "type")
	switch fmt.Sprint(typ) {
	case "1":
		b, _ := field(tv, "bytes")
		s, _ := b.(string)
		bb, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return "", nil, false
		}
		return FormatBytes(kb), FormatBytes(bb), true
	case "2":
		u, _ := field(tv, "uint")
		return FormatBytes(kb), u, true
	}
	return "", nil, false
}
