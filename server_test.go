package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testServer() *server {
	return &server{token: "secret", host: "127.0.0.1:1234", jobs: &jobManager{logger: &fileLogger{}}, logger: &fileLogger{}}
}

func TestAPINeedsTokenAndHost(t *testing.T) {
	h := testServer().routes()
	cases := []struct {
		host, token string
		want        int
	}{
		{"127.0.0.1:1234", "secret", 200},
		{"127.0.0.1:1234", "", 401},
		{"127.0.0.1:1234", "wrong", 401},
		{"evil.example", "secret", 403}, // DNS rebinding
		{"localhost:1234", "secret", 403},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/api/info", nil)
		r.Host = c.host
		if c.token != "" {
			r.Header.Set("X-DragonClone-Token", c.token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("host %q token %q: got %d, want %d", c.host, c.token, w.Code, c.want)
		}
	}
}

func TestStaticFilesServed(t *testing.T) {
	h := testServer().routes()
	for _, p := range []string{"/", "/app.js", "/app.css", "/favicon.svg"} {
		r := httptest.NewRequest("GET", p, nil)
		r.Host = "127.0.0.1:1234"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Errorf("%s: %d", p, w.Code)
		}
		if p == "/" && !strings.Contains(w.Header().Get("Content-Security-Policy"), "default-src 'self'") {
			t.Error("missing CSP on the page")
		}
	}
}

func TestPlanRejectsUnknownKind(t *testing.T) {
	if _, err := makePlan(&JobRequest{Kind: "format-c"}); err == nil {
		t.Fatal("unknown operation accepted")
	}
}

func TestImageNames(t *testing.T) {
	for name, want := range map[string]bool{"a.img": true, "b.IMG.GZ": true, "c.vhd": true, "d.iso": true, "e.txt": false, "f.json": false} {
		if isImageName(name) != want {
			t.Errorf("%s", name)
		}
	}
}
