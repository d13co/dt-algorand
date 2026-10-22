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

package logic

// Stopping a program from a Tracer hook. Non-standard fork feature, kept in its own file to keep
// upstream merges of eval.go clean.

// EvalAborted is a panic value that stops the running program at once. A Tracer hook panics with
// it to stop a program, for instance when the request that runs it is canceled. eval returns it as
// the program's error, without the error log and stack trace a panic otherwise gets.
type EvalAborted struct {
	Cause error
}

func (e EvalAborted) Error() string {
	return "evaluation aborted: " + e.Cause.Error()
}

func (e EvalAborted) Unwrap() error {
	return e.Cause
}
