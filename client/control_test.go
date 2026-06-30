package client

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestRouteControlResponseDelivers(t *testing.T) {
	c := &Client{pending: map[string]chan *controlResponse{}}
	ch := make(chan *controlResponse, 1)
	c.pending["req-7"] = ch

	c.routeControlResponse([]byte(`{"type":"control_response","response":{"subtype":"success","request_id":"req-7","response":{"ok":true}}}`))

	select {
	case resp := <-ch:
		if resp.Response.RequestID != "req-7" || resp.Response.Subtype != "success" {
			t.Errorf("bad response: %+v", resp.Response)
		}
	case <-time.After(time.Second):
		t.Fatal("response not routed")
	}
	// pending entry must be removed
	if _, ok := c.pending["req-7"]; ok {
		t.Error("pending entry not deleted")
	}
}

func TestRouteControlResponseUnknownID(t *testing.T) {
	c := &Client{pending: map[string]chan *controlResponse{}}
	// Should not panic when no waiter is registered.
	c.routeControlResponse([]byte(`{"type":"control_response","response":{"request_id":"nope"}}`))
	// Invalid JSON should be ignored.
	c.routeControlResponse([]byte(`not json`))
}

func TestFailPendingUnblocks(t *testing.T) {
	c := &Client{pending: map[string]chan *controlResponse{}}
	ch := make(chan *controlResponse, 1)
	c.pending["req-1"] = ch
	c.failPending(errTest)
	select {
	case resp := <-ch:
		if resp.Response.Subtype != "error" {
			t.Errorf("expected error subtype, got %+v", resp.Response)
		}
	default:
		t.Fatal("failPending did not deliver")
	}
}

func TestSendControlOnClosed(t *testing.T) {
	c := &Client{pending: map[string]chan *controlResponse{}}
	c.closed.Store(true)
	if _, err := c.sendControl(context.Background(), "interrupt", nil); err == nil {
		t.Error("sendControl on closed client should error")
	}
}

// fakeClientNoAck ignores control_request lines, so control calls time out.
func fakeClientNoAck(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake binary uses /bin/sh")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "claude")
	script := `#!/bin/sh
while IFS= read -r line; do
  case "$line" in
    *control_request*) continue ;;
  esac
  printf '%s\n' '{"type":"result","subtype":"success","session_id":"s","result":"ok"}'
done
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestControlTimeout(t *testing.T) {
	bin := fakeClientNoAck(t)
	c, err := New(context.Background(), Config{Binary: bin, ControlTimeout: 250 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()
	if err := c.Interrupt(context.Background()); err == nil {
		t.Error("Interrupt should time out when not acked")
	}
}

func TestControlContextCancel(t *testing.T) {
	bin := fakeClientNoAck(t)
	c, err := New(context.Background(), Config{Binary: bin})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Initialize(ctx); err == nil {
		t.Error("Initialize with cancelled ctx should error")
	}
}

func TestInitializeAndControlViaFake(t *testing.T) {
	bin := fakeClient(t)
	ctx := context.Background()
	c, err := New(ctx, Config{Binary: bin})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	raw, err := c.Initialize(ctx)
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	// fake acks with success and no response payload.
	if len(raw) != 0 {
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Errorf("init response not valid json: %v", err)
		}
	}
}

var errTest = errStr("boom")

type errStr string

func (e errStr) Error() string { return string(e) }
