package server

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRoutesProbeHandler(t *testing.T) {
	router := mux.NewRouter()
	registerApiRoutes(router)

	originalRoutes := Routes
	t.Cleanup(func() { Routes = originalRoutes })

	t.Run("unknown route returns 404", func(t *testing.T) {
		Routes = NewRoutes()

		req := httptest.NewRequest(http.MethodGet, "/routes/unknown.example.com/probe", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusNotFound, rec.Code)
	})

	t.Run("registered but unreachable backend returns reachable=false", func(t *testing.T) {
		Routes = NewRoutes()
		// Port 1 is reserved (tcpmux); nothing listens, so DialTimeout fails fast.
		Routes.CreateMapping("down.example.com", "127.0.0.1:1", "", nil, nil, "", "")

		req := httptest.NewRequest(http.MethodGet, "/routes/down.example.com/probe", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		require.Equal(t, http.StatusOK, rec.Code)
		var body struct {
			Backend   string `json:"backend"`
			Reachable bool   `json:"reachable"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.Equal(t, "127.0.0.1:1", body.Backend)
		assert.False(t, body.Reachable)
	})

	t.Run("registered reachable backend returns reachable=true", func(t *testing.T) {
		Routes = NewRoutes()

		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		t.Cleanup(func() { _ = listener.Close() })
		go func() {
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				_ = conn.Close()
			}
		}()

		Routes.CreateMapping("up.example.com", listener.Addr().String(), "", nil, nil, "", "")

		req := httptest.NewRequest(http.MethodGet, "/routes/up.example.com/probe", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		require.Equal(t, http.StatusOK, rec.Code)
		var body struct {
			Backend   string `json:"backend"`
			Reachable bool   `json:"reachable"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.Equal(t, listener.Addr().String(), body.Backend)
		assert.True(t, body.Reachable)
	})
}
