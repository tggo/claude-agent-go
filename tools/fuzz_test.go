package tools

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// go test -run '^$' -fuzz '^FuzzRequireBearer$' -fuzztime 30s ./tools

func FuzzRequireBearer(f *testing.F) {
	const token = "s3cr3t-T0ken_abc"
	for _, h := range []string{
		"", "Bearer " + token, "bearer " + token, "Bearer  " + token, "Bearer " + token + " ", " Bearer " + token,
		"Bearer " + token + "x", "Bearer " + token[:len(token)-1], "Bearer", "Bearer ", token, "Basic " + token,
		"Bearer " + token + "\x00", "Bearer " + token + "\r\nX: y", "BEARER " + token, "Bearer\t" + token,
	} {
		f.Add(h)
	}
	f.Fuzz(func(t *testing.T, header string) {
		for _, tok := range []string{token, "", "a b"} {
			called := false
			h := requireBearer(tok, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
			req := httptest.NewRequest("POST", "/mcp", nil)
			req.Header["Authorization"] = []string{header}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			want := header == "Bearer "+tok
			if called != want {
				t.Fatalf("token %q header %q: handler called=%v, want %v", tok, header, called, want)
			}
			if !want && (rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") == "") {
				t.Fatalf("rejection code=%d headers=%v", rec.Code, rec.Header())
			}
		}
		// A second, valid Authorization header must not rescue a bad first one.
		called := false
		h := requireBearer(token, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
		req := httptest.NewRequest("POST", "/mcp", nil)
		req.Header["Authorization"] = []string{header, "Bearer " + token}
		h.ServeHTTP(httptest.NewRecorder(), req)
		if called != (header == "Bearer "+token) {
			t.Fatalf("multi-header: header %q called=%v", header, called)
		}
		// No header at all is always rejected for a non-empty token.
		called = false
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/mcp", nil))
		if called {
			t.Fatal("request without Authorization passed")
		}
	})
}
