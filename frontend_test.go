package isp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFrontendHandler(t *testing.T) {
	handler, err := FrontendHandler()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/", "/settings"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("GET", path, nil))
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "<div id=\"root\"></div>") {
			t.Fatalf("%s: status %d, body %q", path, response.Code, response.Body.String())
		}
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "/missing.js", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("missing asset status %d", response.Code)
	}
}
