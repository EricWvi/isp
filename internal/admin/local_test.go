package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLocalOnly(t *testing.T) {
	handler := LocalOnly(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	tests := []struct {
		name, host, origin string
		want               int
	}{
		{"loopback", "127.0.0.1:8080", "http://127.0.0.1:8080", http.StatusNoContent},
		{"localhost", "localhost:8080", "", http.StatusNoContent},
		{"ipv6", "[::1]:8080", "http://[::1]:8080", http.StatusNoContent},
		{"remote", "example.com:8080", "", http.StatusForbidden},
		{"rebinding", "127.0.0.1.attacker.test:8080", "", http.StatusForbidden},
		{"foreign origin", "127.0.0.1:8080", "http://evil.test", http.StatusForbidden},
		{"different port", "127.0.0.1:8080", "http://127.0.0.1:5173", http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "http://127.0.0.1:8080/api/rotate", nil)
			req.Host = tt.host
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			if response.Code != tt.want {
				t.Fatalf("got %d, want %d", response.Code, tt.want)
			}
		})
	}
}
