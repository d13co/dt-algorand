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
	"fmt"
	"io"
	"time"
)

// Change is a leaf that differs between two flattened values. Old is empty
// with Added, New with Removed.
type Change struct {
	Path, Old, New string
	Added, Removed bool
}

// Diff returns the leaves that differ between old and new, in new's order,
// then the removed ones in old's.
func Diff(old, new []Field) []Change {
	prev := make(map[string]string, len(old))
	for _, f := range old {
		prev[f.Path] = f.Value
	}
	var out []Change
	seen := make(map[string]bool, len(new))
	for _, f := range new {
		seen[f.Path] = true
		o, ok := prev[f.Path]
		switch {
		case !ok:
			out = append(out, Change{Path: f.Path, New: f.Value, Added: true})
		case o != f.Value:
			out = append(out, Change{Path: f.Path, Old: o, New: f.Value})
		}
	}
	for _, f := range old {
		if !seen[f.Path] {
			out = append(out, Change{Path: f.Path, Old: f.Value, Removed: true})
		}
	}
	return out
}

// History writes the versions of objects as text: per version a line with
// the round, its time, the object and what happened to it, then the leaves
// that changed since the object's previous version (or all of them, Full).
// Objects are named by the caller ("app:12", "assets[31]"), and each keeps
// its own previous value, so versions that list only the objects that
// changed in a round, like an account's, diff correctly.
type History struct {
	W    io.Writer
	Opts Options
	// Full prints every leaf of each version instead of its changes.
	Full bool

	prev map[string]*[]Field // nil: absent; missing: unknown
}

// Baseline sets the value of obj before the first version added (nil: it
// did not exist), so that the first version shows as a change too. Without
// one, the first version of an object prints in full.
func (h *History) Baseline(obj string, v any) error {
	if h.prev == nil {
		h.prev = map[string]*[]Field{}
	}
	if v == nil {
		h.prev[obj] = nil
		return nil
	}
	fields, err := Flatten(v, h.Opts)
	if err != nil {
		return err
	}
	h.prev[obj] = &fields
	return nil
}

// Add writes a version of obj at round rnd (timestamp ts, in seconds): a
// deletion, or the new value v. A nil v that is not a deletion is a version
// whose value was not fetched.
func (h *History) Add(rnd uint64, ts int64, obj string, deleted bool, v any) error {
	if h.prev == nil {
		h.prev = map[string]*[]Field{}
	}
	prev, known := h.prev[obj]
	what := "updated"
	switch {
	case deleted:
		what = "deleted"
	case known && prev == nil:
		what = "created"
	case v == nil:
		what = "changed"
	}
	if _, err := fmt.Fprintf(h.W, "%d  %s  %s %s\n", rnd, time.Unix(ts, 0).UTC().Format(time.DateTime), obj, what); err != nil {
		return err
	}
	if deleted {
		h.prev[obj] = nil
		return nil
	}
	if v == nil {
		delete(h.prev, obj) // the next version has nothing to diff against
		return nil
	}
	fields, err := Flatten(v, h.Opts)
	if err != nil {
		return err
	}
	if h.Full || !known || prev == nil {
		err = WriteFields(h.W, fields, "  ")
	} else {
		err = WriteChanges(h.W, Diff(*prev, fields), "  ")
	}
	h.prev[obj] = &fields
	return err
}
