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
package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"

	"github.com/algorand/go-algorand/daemon/algod/api/server/lib"
	"github.com/algorand/go-algorand/daemon/algod/api/server/lib/middlewares"
	"github.com/algorand/go-algorand/daemon/algod/api/server/v1/routes"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/test/partitiontest"
)

func setupRouter() *echo.Echo {
	e := echo.New()
	// Make a deep copy of the routes array with handlers.
	v1RoutesCopy := make([]lib.Route, len(routes.V1Routes))
	for _, route := range routes.V1Routes {
		v1RoutesCopy = append(v1RoutesCopy, lib.Route{
			Name:        route.Name,
			Method:      route.Method,
			Path:        route.Path,
			HandlerFunc: route.HandlerFunc,
		})
	}
	// Make a ReqContext with an initialized logger to prevent nil dereferencing.
	reqCtx := lib.ReqContext{Log: logging.NewLogger()}
	// Registering v1 routes
	registerHandlers(e, apiV1Tag, v1RoutesCopy, reqCtx)
	return e
}

func TestGetTransactionV1Sunset(t *testing.T) {
	partitiontest.PartitionTest(t)

	testCases := []struct {
		path  string
		route string
	}{
		{"/v1/account/address-param/transactions/pending", "/v1/account/:addr/transactions/pending"},
		{"/v1/status/wait-for-block-after/123456", "/v1/status/wait-for-block-after/:round"},
		{"/v1/block/123456", "/v1/block/:round"},
		{"/v1/transactions/pending/ASPB5E72OT2UWSOCQGD5OPT3W4KV4LZZDL7L5MBCC3EBAIJCDHAA", "/v1/transactions/pending/:txid"},
		{"/v1/asset/123456", "/v1/asset/:index"},
	}

	rec := httptest.NewRecorder()
	e := setupRouter()
	ctx := e.NewContext(nil, rec)

	for _, testCase := range testCases {
		e.Router().Find(http.MethodGet, testCase.path, ctx)
		assert.Equal(t, testCase.route, ctx.Path())

		// Check that router correctly routes to the v1Sunset handler.
		assert.Equal(t, nil, ctx.Handler()(ctx))
		assert.NotNil(t, rec.Body)
		assert.Equal(t, http.StatusGone, rec.Code)
	}
}

// TestExposedHeaders checks that cross-origin scripts can read the headers
// clients act on: Retry-After, the effort and the round of a read at a round.
func TestExposedHeaders(t *testing.T) {
	partitiontest.PartitionTest(t)

	e := echo.New()
	e.Use(middlewares.MakeCORS(TokenHeader, exposedHeaders...))
	e.GET("/health", func(c echo.Context) error { return c.NoContent(http.StatusOK) })
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("Origin", "http://algorand.com")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	assert.Equal(t, "Retry-After,X-Algod-Effort,X-Algod-TT-Round", rec.Header().Get("Access-Control-Expose-Headers"))
}

// TestConnectionLimitCORS checks that a browser can read the 429 of a node at its connection limit:
// it carries the CORS headers and Retry-After, and a preflight is answered without taking a slot.
func TestConnectionLimitCORS(t *testing.T) {
	partitiontest.PartitionTest(t)

	e := echo.New()
	useMiddleware(e, logging.TestingLog(t), true, 1)
	entered := make(chan struct{})
	release := make(chan struct{})
	e.GET("/slow", func(c echo.Context) error {
		entered <- struct{}{}
		<-release
		return c.NoContent(http.StatusOK)
	})
	e.GET("/health", func(c echo.Context) error { return c.NoContent(http.StatusOK) })

	const origin = "http://algorand.com"
	serve := func(method, path string, header map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		for k, v := range header {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec
	}

	// Take the only slot.
	done := make(chan *httptest.ResponseRecorder, 1)
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	go func() { done <- serve(http.MethodGet, "/slow", nil) }()
	select {
	case <-entered:
	case rec := <-done:
		t.Fatalf("the request that should hold the slot was answered %d", rec.Code)
	}

	rec := serve(http.MethodGet, "/health", map[string]string{"Origin": origin, "Accept-Encoding": "gzip"})
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Equal(t, "*", rec.Header().Get("Access-Control-Allow-Origin"))
	assert.ElementsMatch(t, exposedHeaders, strings.Split(rec.Header().Get("Access-Control-Expose-Headers"), ","))
	assert.Equal(t, "1", rec.Header().Get("Retry-After"))
	// Refused before Gzip, which would add Vary: Accept-Encoding.
	assert.NotContains(t, rec.Header().Values("Vary"), "Accept-Encoding")
	assert.Empty(t, rec.Header().Get("Content-Encoding"))
	assert.Empty(t, rec.Body.String())

	// Unknown paths and methods count too.
	assert.Equal(t, http.StatusTooManyRequests, serve(http.MethodGet, "/nope", nil).Code)
	assert.Equal(t, http.StatusTooManyRequests, serve(http.MethodPut, "/health", nil).Code)

	rec = serve(http.MethodOptions, "/health", map[string]string{
		"Origin":                                 origin,
		"Access-Control-Request-Method":          http.MethodGet,
		"Access-Control-Request-Headers":         TokenHeader,
		"Access-Control-Request-Private-Network": "true",
	})
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, "*", rec.Header().Get("Access-Control-Allow-Origin"))
	assert.Contains(t, rec.Header().Get("Access-Control-Allow-Methods"), http.MethodGet)
	assert.Contains(t, rec.Header().Get("Access-Control-Allow-Headers"), TokenHeader)
	assert.Equal(t, "true", rec.Header().Get("Access-Control-Allow-Private-Network"))

	// An OPTIONS request that is not a preflight still gets the router's Allow header.
	rec = serve(http.MethodOptions, "/health", nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Contains(t, rec.Header().Get("Allow"), http.MethodGet)

	unblock()
	assert.Equal(t, http.StatusOK, (<-done).Code)
	rec = serve(http.MethodGet, "/health", map[string]string{"Origin": origin})
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Header().Get("Retry-After"))
}
