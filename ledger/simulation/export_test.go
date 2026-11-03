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

package simulation

// WithAfterGroupHook returns a copy of r that runs hook at the end of the tracer's AfterTxnGroup
// for each top-level group, inside the evaluator's TransactionGroup. A panic in hook after a
// successful group happens during the commit, and leaves the evaluator corrupted.
func WithAfterGroupHook(r Request, hook func(g int, evalError error)) Request {
	r.hooks = withHooks(r.hooks)
	r.hooks.afterGroup = hook
	return r
}

// WithAfterTxnHook returns a copy of r that runs hook at the end of the tracer's AfterTxn for each
// top-level txn. A panic in hook happens before the group is committed, and leaves the evaluator
// usable.
func WithAfterTxnHook(r Request, hook func(g, i int)) Request {
	r.hooks = withHooks(r.hooks)
	r.hooks.afterTxn = hook
	return r
}

// WithBeforeTxnHook returns a copy of r that runs hook at the end of the tracer's BeforeTxn for
// each top-level txn. A panic in hook happens inside the txn, as a panic while applying it would,
// and leaves the evaluator usable.
func WithBeforeTxnHook(r Request, hook func(g, i int)) Request {
	r.hooks = withHooks(r.hooks)
	r.hooks.beforeTxn = hook
	return r
}

func withHooks(h *testHooks) *testHooks {
	if h == nil {
		return &testHooks{}
	}
	c := *h
	return &c
}
