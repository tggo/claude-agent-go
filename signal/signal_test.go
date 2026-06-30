package signal

import "testing"

func TestSetDetect(t *testing.T) {
	tests := []struct {
		name string
		set  Set
		text string
		want string
	}{
		{
			name: "no match returns empty",
			set: Set{
				Markers:  map[string]string{"done": "<<<DONE>>>"},
				Priority: []string{"done"},
			},
			text: "nothing here",
			want: "",
		},
		{
			name: "single priority match",
			set: Set{
				Markers:  map[string]string{"done": "<<<DONE>>>"},
				Priority: []string{"done"},
			},
			text: "work <<<DONE>>> finished",
			want: "done",
		},
		{
			name: "priority ordering: earliest in Priority wins",
			set: Set{
				Markers:  map[string]string{"done": "<<<DONE>>>", "fail": "<<<FAIL>>>"},
				Priority: []string{"fail", "done"},
			},
			text: "<<<DONE>>> and <<<FAIL>>>",
			want: "fail",
		},
		{
			name: "priority ordering reversed",
			set: Set{
				Markers:  map[string]string{"done": "<<<DONE>>>", "fail": "<<<FAIL>>>"},
				Priority: []string{"done", "fail"},
			},
			text: "<<<DONE>>> and <<<FAIL>>>",
			want: "done",
		},
		{
			name: "marker not in Priority still detected",
			set: Set{
				Markers:  map[string]string{"extra": "<<<EXTRA>>>"},
				Priority: nil,
			},
			text: "an <<<EXTRA>>> marker",
			want: "extra",
		},
		{
			name: "priority name missing from markers is skipped",
			set: Set{
				Markers:  map[string]string{"done": "<<<DONE>>>"},
				Priority: []string{"ghost", "done"},
			},
			text: "<<<DONE>>>",
			want: "done",
		},
		{
			name: "empty marker string ignored in priority",
			set: Set{
				Markers:  map[string]string{"done": ""},
				Priority: []string{"done"},
			},
			text: "anything",
			want: "",
		},
		{
			name: "empty marker string ignored in non-priority",
			set: Set{
				Markers:  map[string]string{"done": ""},
				Priority: nil,
			},
			text: "anything",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.set.Detect(tt.text); got != tt.want {
				t.Errorf("Detect(%q) = %q, want %q", tt.text, got, tt.want)
			}
		})
	}
}

func TestContainsAny(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		phrases []string
		want    bool
	}{
		{
			name:    "empty phrases returns false",
			text:    "some text",
			phrases: nil,
			want:    false,
		},
		{
			name:    "match found",
			text:    "there is nothing to commit here",
			phrases: []string{"nothing to commit"},
			want:    true,
		},
		{
			name:    "case-insensitive match",
			text:    "Already Implemented in the codebase",
			phrases: []string{"already implemented"},
			want:    true,
		},
		{
			name:    "case-insensitive match other direction",
			text:    "already implemented",
			phrases: []string{"ALREADY IMPLEMENTED"},
			want:    true,
		},
		{
			name:    "no match",
			text:    "fresh changes",
			phrases: []string{"nothing to commit", "already done"},
			want:    false,
		},
		{
			name:    "second phrase matches",
			text:    "this is already done",
			phrases: []string{"nothing to commit", "already done"},
			want:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ContainsAny(tt.text, tt.phrases); got != tt.want {
				t.Errorf("ContainsAny(%q, %v) = %v, want %v", tt.text, tt.phrases, got, tt.want)
			}
		})
	}
}
