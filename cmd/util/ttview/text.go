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
	"strings"
	"text/tabwriter"
)

func newTabWriter(w io.Writer) *tabwriter.Writer {
	return tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
}

// WriteFields writes one "path  value" line per field, aligned, each line
// starting with indent.
func WriteFields(w io.Writer, fields []Field, indent string) error {
	tw := newTabWriter(w)
	for _, f := range fields {
		fmt.Fprintf(tw, "%s%s\t%s\n", indent, f.Path, f.Value)
	}
	return tw.Flush()
}

// WriteChanges writes one "path  old -> new" line per change, aligned, each
// line starting with indent. "(none)" stands for an absent leaf.
func WriteChanges(w io.Writer, changes []Change, indent string) error {
	tw := newTabWriter(w)
	for _, c := range changes {
		old, new := c.Old, c.New
		if c.Added {
			old = "(none)"
		}
		if c.Removed {
			new = "(none)"
		}
		fmt.Fprintf(tw, "%s%s\t%s -> %s\n", indent, c.Path, old, new)
	}
	return tw.Flush()
}

// WriteTable writes an aligned table with an upper-cased header row.
func WriteTable(w io.Writer, header []string, rows [][]string) error {
	tw := newTabWriter(w)
	if len(header) > 0 {
		fmt.Fprintln(tw, strings.ToUpper(strings.Join(header, "\t")))
	}
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	return tw.Flush()
}
