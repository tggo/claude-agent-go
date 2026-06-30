package claudecli

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtractJSONFromText_EmbeddedArray(t *testing.T) {
	text := `Here is the analysis result:
[{"name": "test", "value": 42}]
That's the output.`

	raw, err := ExtractJSONFromText(text)
	require.NoError(t, err)
	assert.JSONEq(t, `[{"name": "test", "value": 42}]`, string(raw))
}

func TestExtractJSONFromText_PureJSON(t *testing.T) {
	text := `[{"key": "value"}]`

	raw, err := ExtractJSONFromText(text)
	require.NoError(t, err)
	assert.JSONEq(t, `[{"key": "value"}]`, string(raw))
}

func TestExtractJSONFromText_NoJSON(t *testing.T) {
	text := `This text contains no JSON arrays at all.`

	_, err := ExtractJSONFromText(text)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no JSON array found")
}

func TestExtractJSONFromText_InvalidJSON(t *testing.T) {
	text := `Here is broken JSON: [{"key": value}]`

	_, err := ExtractJSONFromText(text)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not valid JSON")
}

func TestExtractJSONFromText_NestedArrays(t *testing.T) {
	text := `Result: [{"items": [1, 2, 3], "nested": [{"a": 1}]}]`

	raw, err := ExtractJSONFromText(text)
	require.NoError(t, err)
	assert.JSONEq(t, `[{"items": [1, 2, 3], "nested": [{"a": 1}]}]`, string(raw))
}

func TestExtractJSONFromText_EmptyArray(t *testing.T) {
	text := `No issues found: []`

	raw, err := ExtractJSONFromText(text)
	require.NoError(t, err)
	assert.JSONEq(t, `[]`, string(raw))
}

func TestExtractJSONObjectFromText_EmbeddedObject(t *testing.T) {
	text := `Analysis complete. Here is the result:
{"complexity": "high", "score": 0.8, "summary": "Complex feature"}
End of analysis.`

	raw, err := ExtractJSONObjectFromText(text)
	require.NoError(t, err)
	assert.JSONEq(t, `{"complexity": "high", "score": 0.8, "summary": "Complex feature"}`, string(raw))
}

func TestExtractJSONObjectFromText_PureJSON(t *testing.T) {
	text := `{"key": "value", "num": 42}`

	raw, err := ExtractJSONObjectFromText(text)
	require.NoError(t, err)
	assert.JSONEq(t, `{"key": "value", "num": 42}`, string(raw))
}

func TestExtractJSONObjectFromText_NoJSON(t *testing.T) {
	text := `No JSON objects here.`

	_, err := ExtractJSONObjectFromText(text)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no JSON object found")
}

func TestExtractJSONObjectFromText_InvalidJSON(t *testing.T) {
	text := `Broken: {key: no_quotes}`

	_, err := ExtractJSONObjectFromText(text)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not valid JSON")
}

func TestExtractJSONObjectFromText_NestedObjects(t *testing.T) {
	text := `Output: {"outer": {"inner": {"deep": true}}, "list": [1, 2]}`

	raw, err := ExtractJSONObjectFromText(text)
	require.NoError(t, err)
	assert.JSONEq(t, `{"outer": {"inner": {"deep": true}}, "list": [1, 2]}`, string(raw))
}

func TestUnmarshalJSONObjectFromText_SimpleStruct(t *testing.T) {
	type testResult struct {
		Name  string  `json:"name"`
		Score float64 `json:"score"`
		Valid bool    `json:"valid"`
	}

	text := `Here is the result: {"name": "test", "score": 0.95, "valid": true} done.`

	result, err := UnmarshalJSONObjectFromText[testResult](text)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, "test", result.Name)
	assert.InDelta(t, 0.95, result.Score, 0.001)
	assert.True(t, result.Valid)
}

func TestUnmarshalJSONObjectFromText_NoJSON(t *testing.T) {
	type testResult struct {
		Name string `json:"name"`
	}

	text := `No JSON here at all.`

	result, err := UnmarshalJSONObjectFromText[testResult](text)
	assert.Error(t, err)
	assert.Nil(t, result)
}

func TestUnmarshalJSONObjectFromText_TypeMismatch(t *testing.T) {
	type testResult struct {
		Count int `json:"count"`
	}

	// JSON has a string where int is expected - json.Unmarshal will fail
	text := `{"count": "not_a_number"}`

	result, err := UnmarshalJSONObjectFromText[testResult](text)
	assert.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "failed to unmarshal JSON")
}

func TestExtractJSONObjectFromText_MultipleObjects(t *testing.T) {
	// Should extract the first object
	text := `First: {"a": 1} Second: {"b": 2}`

	raw, err := ExtractJSONObjectFromText(text)
	require.NoError(t, err)
	assert.JSONEq(t, `{"a": 1}`, string(raw))
}

func TestExtractJSONFromText_MultipleArrays(t *testing.T) {
	// Should extract the first array
	text := `First: [1, 2] Second: [3, 4]`

	raw, err := ExtractJSONFromText(text)
	require.NoError(t, err)
	assert.JSONEq(t, `[1, 2]`, string(raw))
}
