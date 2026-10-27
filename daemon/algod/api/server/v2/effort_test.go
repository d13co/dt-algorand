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

package v2

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/test/partitiontest"
)

// TestWriteJSON checks that writeJSON writes what ctx.JSON does, pretty or not.
func TestWriteJSON(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	v := map[string]any{"round": 7, "versions": []string{"a", "<b>"}, "next-token": nil}
	for _, target := range []string{"/", "/?pretty"} {
		write := func(f func(echo.Context) error) *httptest.ResponseRecorder {
			rec := httptest.NewRecorder()
			require.NoError(t, f(echo.New().NewContext(httptest.NewRequest(http.MethodGet, target, nil), rec)))
			return rec
		}
		want := write(func(ctx echo.Context) error { return ctx.JSON(http.StatusOK, v) })
		got := write(func(ctx echo.Context) error { return writeJSON(ctx, v) })
		require.Equal(t, want.Code, got.Code, target)
		require.Equal(t, want.Header(), got.Header(), target)
		require.Equal(t, want.Body.String(), got.Body.String(), target)
		require.Equal(t, target != "/", bytes.Contains(got.Body.Bytes(), []byte("\n  ")), target)
	}
}
