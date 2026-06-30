package client

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// defaultCallbackTimeout bounds how long a user permission/hook callback may run
// before the SDK gives up and lets the agent's turn proceed.
const defaultCallbackTimeout = 60 * time.Second

// This file implements the *incoming* half of the control protocol: requests
// the CLI sends TO the SDK while a turn is running — tool-permission decisions
// (can_use_tool) and hook callbacks (PreToolUse/PostToolUse/…). The exact wire
// shapes were taken from the Python SDK (_internal/query.py) and verified
// against the real binary (CLI 2.1.196): the #469 "CLI doesn't emit
// can_use_tool" bug is fixed in this version.

// PermissionContext carries the metadata the CLI attaches to a permission
// request, beyond the tool name and input.
type PermissionContext struct {
	ToolUseID   string            // the tool_use id this decision applies to
	DisplayName string            // human-readable tool name
	Description string            // short description of the call
	Suggestions []json.RawMessage // raw permission_suggestions (e.g. setMode)
}

// PermissionResult is what a CanUseToolFunc returns. Build it with Allow,
// AllowWithInput, or Deny.
type PermissionResult struct {
	behavior     string          // "allow" | "deny"
	updatedInput json.RawMessage // allow: replacement args (nil = keep original)
	message      string          // deny: reason shown to the model
	interrupt    bool            // deny: also interrupt the turn
}

// Allow permits the tool call with its original input.
func Allow() PermissionResult { return PermissionResult{behavior: "allow"} }

// AllowWithInput permits the call but replaces the tool arguments.
func AllowWithInput(updated json.RawMessage) PermissionResult {
	return PermissionResult{behavior: "allow", updatedInput: updated}
}

// Deny blocks the tool call; message is surfaced to the model.
func Deny(message string) PermissionResult {
	return PermissionResult{behavior: "deny", message: message}
}

// DenyAndInterrupt blocks the call and interrupts the current turn.
func DenyAndInterrupt(message string) PermissionResult {
	return PermissionResult{behavior: "deny", message: message, interrupt: true}
}

// toResponseData renders the result into the control_response payload the CLI
// expects, defaulting updatedInput to the original input on allow.
func (p PermissionResult) toResponseData(originalInput json.RawMessage) map[string]any {
	if p.behavior == "deny" {
		d := map[string]any{"behavior": "deny", "message": p.message}
		if p.interrupt {
			d["interrupt"] = true
		}
		return d
	}
	in := p.updatedInput
	if len(in) == 0 {
		in = originalInput
	}
	return map[string]any{"behavior": "allow", "updatedInput": json.RawMessage(in)}
}

// CanUseToolFunc decides whether a tool call may proceed. It runs inline while
// the agent's turn is paused waiting for the decision, so it should return
// promptly. Returning an error denies the call with the error text.
type CanUseToolFunc func(ctx context.Context, toolName string, input json.RawMessage, pctx PermissionContext) (PermissionResult, error)

// HookCallback handles a single hook event. input is the event-specific JSON
// payload (e.g. the tool name and args for PreToolUse); toolUseID may be empty.
// The returned JSON becomes the hook's control_response payload — e.g.
// {"decision":"block","reason":"…"} or {"hookSpecificOutput":{…}}. Returning
// nil means "no opinion"; returning an error fails the hook.
type HookCallback func(ctx context.Context, input json.RawMessage, toolUseID string) (json.RawMessage, error)

// HookMatcher binds callbacks to a tool/event matcher (e.g. matcher "Bash" for
// the "PreToolUse" event). An empty Matcher matches all.
type HookMatcher struct {
	Matcher   string
	Callbacks []HookCallback
}

// incomingControlRequest is a control_request the CLI sends to the SDK.
type incomingControlRequest struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id"`
	Request   struct {
		Subtype     string            `json:"subtype"`
		ToolName    string            `json:"tool_name,omitempty"`
		DisplayName string            `json:"display_name,omitempty"`
		Description string            `json:"description,omitempty"`
		Input       json.RawMessage   `json:"input,omitempty"`
		ToolUseID   string            `json:"tool_use_id,omitempty"`
		Suggestions []json.RawMessage `json:"permission_suggestions,omitempty"`
		CallbackID  string            `json:"callback_id,omitempty"`
	} `json:"request"`
}

// handleControlRequest dispatches an incoming control_request to the configured
// permission or hook callback and writes the control_response. Unknown or
// unhandled subtypes get an error response so the CLI does not hang.
func (c *Client) handleControlRequest(line []byte) {
	var req incomingControlRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return
	}

	data, err := c.dispatchControl(req)
	if err != nil {
		c.writeControlError(req.RequestID, err.Error())
		return
	}
	c.writeControlSuccess(req.RequestID, data)
}

func (c *Client) dispatchControl(req incomingControlRequest) (map[string]any, error) {
	switch req.Request.Subtype {
	case "can_use_tool":
		if c.cfg.CanUseTool == nil {
			return nil, errNoPermissionCallback
		}
		return c.runCallback(func(ctx context.Context) (map[string]any, error) {
			res, err := c.cfg.CanUseTool(ctx, req.Request.ToolName, req.Request.Input, PermissionContext{
				ToolUseID:   req.Request.ToolUseID,
				DisplayName: req.Request.DisplayName,
				Description: req.Request.Description,
				Suggestions: req.Request.Suggestions,
			})
			if err != nil {
				return nil, err
			}
			return res.toResponseData(req.Request.Input), nil
		})

	case "hook_callback":
		c.pendingMu.Lock()
		cb := c.hookCallbacks[req.Request.CallbackID]
		c.pendingMu.Unlock()
		if cb == nil {
			return nil, errNoHookCallback
		}
		return c.runCallback(func(ctx context.Context) (map[string]any, error) {
			out, err := cb(ctx, req.Request.Input, req.Request.ToolUseID)
			if err != nil {
				return nil, err
			}
			return rawToMap(out), nil
		})

	default:
		return nil, errUnsupportedControl
	}
}

// runCallback runs a user permission/hook callback with a bound timeout, so a
// hung callback can't stall the agent's turn forever. On timeout it returns an
// error (delivered to the CLI as a control_response error, letting the turn
// proceed); the callback goroutine is abandoned.
func (c *Client) runCallback(fn func(context.Context) (map[string]any, error)) (map[string]any, error) {
	d := c.cfg.CallbackTimeout
	if d <= 0 {
		d = defaultCallbackTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()

	type result struct {
		data map[string]any
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		data, err := fn(ctx)
		ch <- result{data, err}
	}()

	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("callback timed out after %v", d)
	case r := <-ch:
		return r.data, r.err
	}
}

// rawToMap turns a JSON object (a hook's raw output) into a map for embedding in
// the response. A nil/empty payload becomes an empty object ("no opinion").
func rawToMap(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return map[string]any{}
	}
	return m
}

var (
	errNoPermissionCallback = errStr2("can_use_tool received but no CanUseTool callback configured")
	errNoHookCallback       = errStr2("hook_callback received for unknown callback id")
	errUnsupportedControl   = errStr2("unsupported control request subtype")
)

type errStr2 string

func (e errStr2) Error() string { return string(e) }
