package client

// HookBuilder is a fluent way to assemble the Config.Hooks map without writing
// the nested map/slice literal by hand. The matcher string is whatever the CLI
// accepts for that event (a tool name, a glob, or a regex).
//
//	cfg.Hooks = client.NewHooks().
//	    PreToolUse("Bash", denyDangerousBash).
//	    PostToolUse("", auditEveryTool).
//	    Build()
type HookBuilder struct {
	hooks map[string][]HookMatcher
}

// NewHooks starts a HookBuilder.
func NewHooks() *HookBuilder {
	return &HookBuilder{hooks: map[string][]HookMatcher{}}
}

// On registers cb for an arbitrary hook event and matcher. Multiple callbacks
// with the same event+matcher are appended in order.
func (b *HookBuilder) On(event, matcher string, cb HookCallback) *HookBuilder {
	list := b.hooks[event]
	for i := range list {
		if list[i].Matcher == matcher {
			list[i].Callbacks = append(list[i].Callbacks, cb)
			b.hooks[event] = list
			return b
		}
	}
	b.hooks[event] = append(list, HookMatcher{Matcher: matcher, Callbacks: []HookCallback{cb}})
	return b
}

// PreToolUse registers a hook that runs before a matching tool fires.
func (b *HookBuilder) PreToolUse(matcher string, cb HookCallback) *HookBuilder {
	return b.On("PreToolUse", matcher, cb)
}

// PostToolUse registers a hook that runs after a matching tool completes.
func (b *HookBuilder) PostToolUse(matcher string, cb HookCallback) *HookBuilder {
	return b.On("PostToolUse", matcher, cb)
}

// Build returns the assembled hooks map for Config.Hooks.
func (b *HookBuilder) Build() map[string][]HookMatcher {
	return b.hooks
}
