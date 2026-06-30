package claudecli

import (
	"encoding/json"
	"fmt"
)

// ExtractJSONFromText finds and extracts the first JSON array from text.
// Useful for parsing Claude responses that include JSON arrays among other text.
func ExtractJSONFromText(text string) (json.RawMessage, error) {
	start := -1
	end := -1
	bracketCount := 0

	for i, c := range text {
		if c == '[' {
			if start == -1 {
				start = i
			}
			bracketCount++
		} else if c == ']' {
			bracketCount--
			if bracketCount == 0 && start != -1 {
				end = i + 1
				break
			}
		}
	}

	if start == -1 || end == -1 {
		return nil, fmt.Errorf("no JSON array found in text")
	}

	raw := json.RawMessage(text[start:end])
	// Validate it's valid JSON
	if !json.Valid(raw) {
		return nil, fmt.Errorf("extracted text is not valid JSON")
	}

	return raw, nil
}

// ExtractJSONObjectFromText finds and extracts the first JSON object from text.
// Useful for parsing Claude responses that include JSON objects among other text.
func ExtractJSONObjectFromText(text string) (json.RawMessage, error) {
	start := -1
	end := -1
	braceCount := 0

	for i, c := range text {
		if c == '{' {
			if start == -1 {
				start = i
			}
			braceCount++
		} else if c == '}' {
			braceCount--
			if braceCount == 0 && start != -1 {
				end = i + 1
				break
			}
		}
	}

	if start == -1 || end == -1 {
		return nil, fmt.Errorf("no JSON object found in text")
	}

	raw := json.RawMessage(text[start:end])
	if !json.Valid(raw) {
		return nil, fmt.Errorf("extracted text is not valid JSON")
	}

	return raw, nil
}

// UnmarshalJSONObjectFromText extracts a JSON object from text and unmarshals it into target.
// Convenience wrapper combining ExtractJSONObjectFromText and json.Unmarshal.
func UnmarshalJSONObjectFromText[T any](text string) (*T, error) {
	raw, err := ExtractJSONObjectFromText(text)
	if err != nil {
		return nil, err
	}

	var result T
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal JSON: %w", err)
	}

	return &result, nil
}
