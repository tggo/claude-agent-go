package client

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// The control protocol is a request/response side-channel multiplexed over the
// same stream-json stdin/stdout as conversational turns. The SDK sends
// control_request lines ({"type":"control_request","request_id":...,
// "request":{"subtype":...}}); the CLI replies with a control_response carrying
// the same request_id. This is empirically verified against the real binary
// (initialize and interrupt subtypes).

// controlResponse is the CLI's reply to a control_request.
type controlResponse struct {
	Type     string `json:"type"`
	Response struct {
		Subtype   string          `json:"subtype"` // "success" | "error"
		RequestID string          `json:"request_id"`
		Error     string          `json:"error,omitempty"`
		Response  json.RawMessage `json:"response,omitempty"`
	} `json:"response"`
}

// controlRequest is the stdin envelope for a control message.
type controlRequest struct {
	Type      string         `json:"type"`
	RequestID string         `json:"request_id"`
	Request   map[string]any `json:"request"`
}

// defaultControlTimeout bounds how long sendControl waits for an ack.
const defaultControlTimeout = 30 * time.Second

// routeControlResponse delivers a control_response line to the goroutine
// waiting on its request_id, if any.
func (c *Client) routeControlResponse(line []byte) {
	var resp controlResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return
	}
	id := resp.Response.RequestID
	c.pendingMu.Lock()
	ch, ok := c.pending[id]
	if ok {
		delete(c.pending, id)
	}
	c.pendingMu.Unlock()
	if ok {
		ch <- &resp
	}
}

// failPending unblocks every waiter when the process ends.
func (c *Client) failPending(err error) {
	c.pendingMu.Lock()
	for id, ch := range c.pending {
		delete(c.pending, id)
		// non-blocking: channels are buffered(1)
		select {
		case ch <- &controlResponse{Response: struct {
			Subtype   string          `json:"subtype"`
			RequestID string          `json:"request_id"`
			Error     string          `json:"error,omitempty"`
			Response  json.RawMessage `json:"response,omitempty"`
		}{Subtype: "error", RequestID: id, Error: err.Error()}}:
		default:
		}
	}
	c.pendingMu.Unlock()
}

// sendControl writes a control_request and blocks until the matching
// control_response arrives, the context is cancelled, the control timeout
// elapses, or the process exits. extra fields are merged into the request body
// alongside subtype.
func (c *Client) sendControl(ctx context.Context, subtype string, extra map[string]any) (*controlResponse, error) {
	if c.closed.Load() {
		return nil, fmt.Errorf("client is closed")
	}

	id := "req-" + strconv.FormatInt(c.reqCounter.Add(1), 10)
	body := map[string]any{"subtype": subtype}
	for k, v := range extra {
		body[k] = v
	}

	ch := make(chan *controlResponse, 1)
	c.pendingMu.Lock()
	c.pending[id] = ch
	c.pendingMu.Unlock()

	if err := c.writeLine(controlRequest{Type: "control_request", RequestID: id, Request: body}); err != nil {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return nil, err
	}

	d := c.cfg.ControlTimeout
	if d <= 0 {
		d = defaultControlTimeout
	}
	timeout := time.NewTimer(d)
	defer timeout.Stop()

	select {
	case <-ctx.Done():
		c.clearPending(id)
		return nil, ctx.Err()
	case <-timeout.C:
		c.clearPending(id)
		return nil, fmt.Errorf("control request %q timed out", subtype)
	case resp := <-ch:
		if resp.Response.Subtype == "error" {
			return resp, fmt.Errorf("control request %q failed: %s", subtype, resp.Response.Error)
		}
		return resp, nil
	}
}

// outgoingControlResponse is the SDK's reply to an incoming control_request.
type outgoingControlResponse struct {
	Type     string         `json:"type"`
	Response map[string]any `json:"response"`
}

func (c *Client) writeControlSuccess(requestID string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	_ = c.writeLine(outgoingControlResponse{
		Type: "control_response",
		Response: map[string]any{
			"subtype":    "success",
			"request_id": requestID,
			"response":   data,
		},
	})
}

func (c *Client) writeControlError(requestID, msg string) {
	_ = c.writeLine(outgoingControlResponse{
		Type: "control_response",
		Response: map[string]any{
			"subtype":    "error",
			"request_id": requestID,
			"error":      msg,
		},
	})
}

func (c *Client) clearPending(id string) {
	c.pendingMu.Lock()
	delete(c.pending, id)
	c.pendingMu.Unlock()
}

// Initialize performs the control-protocol initialize handshake and returns the
// CLI's raw capability response (which includes available slash commands). When
// Config.Hooks are set, it registers their callbacks and declares them to the
// CLI here. New calls this automatically when hooks or a permission callback are
// configured; calling it manually otherwise is optional.
func (c *Client) Initialize(ctx context.Context) (json.RawMessage, error) {
	resp, err := c.sendControl(ctx, "initialize", c.initRequest())
	if err != nil {
		return nil, err
	}
	return resp.Response.Response, nil
}

// initRequest builds the initialize request body (minus subtype, added by
// sendControl): the hooks config plus optional inline agents and skills
// allowlist.
func (c *Client) initRequest() map[string]any {
	req := map[string]any{"hooks": c.buildHooksConfig()}
	if len(c.cfg.Agents) > 0 {
		req["agents"] = c.cfg.Agents
	}
	if c.cfg.Skills != nil {
		req["skills"] = c.cfg.Skills
	}
	return req
}

// buildHooksConfig converts Config.Hooks into the wire format the CLI expects,
// assigning a stable callback id to each callback and recording it for dispatch.
// Returns nil when no hooks are configured.
func (c *Client) buildHooksConfig() map[string]any {
	if len(c.cfg.Hooks) == 0 {
		return nil
	}
	out := map[string]any{}
	for event, matchers := range c.cfg.Hooks {
		var entries []map[string]any
		for _, m := range matchers {
			var ids []string
			for _, cb := range m.Callbacks {
				id := "hook_" + strconv.FormatInt(c.reqCounter.Add(1), 10)
				c.pendingMu.Lock()
				c.hookCallbacks[id] = cb
				c.pendingMu.Unlock()
				ids = append(ids, id)
			}
			entries = append(entries, map[string]any{
				"matcher":         m.Matcher,
				"hookCallbackIds": ids,
			})
		}
		out[event] = entries
	}
	return out
}

// Interrupt asks the CLI to stop the current turn and waits for its
// acknowledgement (a control_response). The in-flight Query then observes the
// turn's terminal (error) result. Safe to call from another goroutine.
func (c *Client) Interrupt(ctx context.Context) error {
	_, err := c.sendControl(ctx, "interrupt", nil)
	return err
}
