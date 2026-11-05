package tools

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestServeValidation(t *testing.T) {
	ok := Tool{Name: "x", Description: "d", Handler: func(context.Context, json.RawMessage) (string, error) { return "", nil }}

	if _, err := Serve(""); err == nil {
		t.Error("empty server name should error")
	}
	if _, err := Serve("s"); err == nil {
		t.Error("no tools should error")
	}
	if _, err := Serve("s", Tool{Description: "d", Handler: ok.Handler}); err == nil {
		t.Error("empty tool name should error")
	}
	if _, err := Serve("s", Tool{Name: "x"}); err == nil {
		t.Error("nil handler should error")
	}
	s, err := Serve("s", ok)
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	defer s.Close()

	if !strings.HasPrefix(s.URL(), "http://127.0.0.1:") {
		t.Errorf("URL = %q", s.URL())
	}
	if s.Name() != "s" {
		t.Errorf("Name = %q", s.Name())
	}
	name, cfg := s.Config()
	if name != "s" || cfg.Type != "http" || cfg.URL != s.URL() {
		t.Errorf("Config = %q %+v", name, cfg)
	}
	if err := s.Close(); err != nil { // idempotent
		t.Errorf("second Close: %v", err)
	}
}

// TestServeRoundTrip exercises the full handler path by connecting a real MCP
// client to the in-process HTTP server — no claude binary required.
func TestServeRoundTrip(t *testing.T) {
	echo := Tool{
		Name:        "echo",
		Description: "echo back the message field",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"message":{"type":"string"}},"required":["message"]}`),
		Handler: func(_ context.Context, args json.RawMessage) (string, error) {
			var in struct {
				Message string `json:"message"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return "", err
			}
			return "echo: " + in.Message, nil
		},
	}
	boom := Tool{
		Name:        "boom",
		Description: "always fails",
		Handler:     func(context.Context, json.RawMessage) (string, error) { return "", errors.New("kaboom") },
	}

	s, err := Serve("test-tools", echo, boom)
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	defer s.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cli := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test-client", Version: "0"}, nil)
	sess, err := cli.Connect(ctx, &mcpsdk.StreamableClientTransport{Endpoint: s.URL(), HTTPClient: authClient(s.Token())}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer sess.Close()

	// list tools
	lt, err := sess.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(lt.Tools) != 2 {
		t.Fatalf("tools = %d, want 2", len(lt.Tools))
	}

	// successful call
	res, err := sess.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "echo",
		Arguments: map[string]any{"message": "hi"},
	})
	if err != nil {
		t.Fatalf("CallTool echo: %v", err)
	}
	if res.IsError {
		t.Fatalf("echo unexpectedly errored: %+v", res)
	}
	if got := textOf(res); got != "echo: hi" {
		t.Errorf("echo text = %q", got)
	}

	// error path
	res, err = sess.CallTool(ctx, &mcpsdk.CallToolParams{Name: "boom", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("CallTool boom transport err: %v", err)
	}
	if !res.IsError {
		t.Error("boom should be IsError")
	}
	if got := textOf(res); !strings.Contains(got, "kaboom") {
		t.Errorf("boom text = %q", got)
	}
}

func textOf(res *mcpsdk.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcpsdk.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

type bearerRT struct{ tok string }

func (b bearerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.tok)
	return http.DefaultTransport.RoundTrip(r)
}

func authClient(tok string) *http.Client { return &http.Client{Transport: bearerRT{tok}} }

func TestBearerAuth(t *testing.T) {
	ok := Tool{Name: "x", Description: "d", Handler: func(context.Context, json.RawMessage) (string, error) { return "hi", nil }}
	s, err := Serve("auth", ok)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if len(s.Token()) != 64 {
		t.Fatalf("token len = %d, want 64 hex chars (256 bit)", len(s.Token()))
	}
	s2, _ := Serve("auth2", ok)
	defer s2.Close()
	if s.Token() == s2.Token() {
		t.Error("tokens must be per-server random")
	}

	for name, hdr := range map[string]string{"none": "", "wrong": "Bearer nope", "no-scheme": s.Token()} {
		req, _ := http.NewRequest("POST", s.URL(), strings.NewReader("{}"))
		if hdr != "" {
			req.Header.Set("Authorization", hdr)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", name, resp.StatusCode)
		}
	}

	// Unauthenticated MCP client cannot connect.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cli := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "c", Version: "0"}, nil)
	if sess, err := cli.Connect(ctx, &mcpsdk.StreamableClientTransport{Endpoint: s.URL()}, nil); err == nil {
		sess.Close()
		t.Fatal("unauthenticated connect succeeded")
	}

	// Config carries the header, for local and remote.
	_, cfg := s.Config()
	if cfg.Headers["Authorization"] != "Bearer "+s.Token() {
		t.Errorf("Config headers = %v", cfg.Headers)
	}
	_, rcfg := s.ConfigForHost("h")
	if rcfg.Headers["Authorization"] != "Bearer "+s.Token() {
		t.Errorf("ConfigForHost headers = %v", rcfg.Headers)
	}
	// A client built from the config's headers works.
	sess, err := cli.Connect(ctx, &mcpsdk.StreamableClientTransport{Endpoint: s.URL(), HTTPClient: authClient(strings.TrimPrefix(cfg.Headers["Authorization"], "Bearer "))}, nil)
	if err != nil {
		t.Fatalf("authed connect: %v", err)
	}
	sess.Close()
}

func TestServeMCP(t *testing.T) {
	type in struct{ A int }
	type out struct{ B int }
	reg := NewRegistry("svc")
	Register(reg, "t", "d", func(context.Context, in) (out, error) { return out{}, nil })
	dir := filepath.Join(t.TempDir(), "runs", "x")
	path, closeFn, err := reg.ServeMCP(dir)
	if err != nil {
		t.Fatal(err)
	}
	di, _ := os.Stat(dir)
	fi, _ := os.Stat(path)
	if di.Mode().Perm() != 0o700 || fi.Mode().Perm() != 0o600 {
		t.Errorf("perms dir=%o file=%o", di.Mode().Perm(), fi.Mode().Perm())
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(b2s(b), "Bearer ") || !strings.Contains(b2s(b), `"svc"`) {
		t.Errorf("config = %s", b)
	}
	if err := closeFn(); err != nil {
		t.Error(err)
	}

	// errors: empty registry, unusable dir (a file in the way).
	if _, _, err := NewRegistry("e").ServeMCP(t.TempDir()); err == nil {
		t.Error("empty registry should error")
	}
	blocker := filepath.Join(t.TempDir(), "f")
	_ = os.WriteFile(blocker, nil, 0o600)
	reg2 := NewRegistry("e2")
	Register(reg2, "t", "d", func(context.Context, in) (out, error) { return out{}, nil })
	if _, _, err := reg2.ServeMCP(filepath.Join(blocker, "sub")); err == nil {
		t.Error("mkdir under a file should error")
	}
	reg3 := NewRegistry("e3")
	Register(reg3, "t", "d", func(context.Context, in) (out, error) { return out{}, nil })
	d3 := t.TempDir()
	_ = os.Mkdir(filepath.Join(d3, "mcp.json"), 0o700) // WriteConfig target is a directory
	if _, _, err := reg3.ServeMCP(d3); err == nil {
		t.Error("write failure should error")
	}
}

func b2s(b []byte) string { return string(b) }
