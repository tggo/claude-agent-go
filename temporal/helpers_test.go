package temporal

import (
	"encoding/json"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
)

type (
	activityInfo  = activity.Info
	encodedValues = converter.EncodedValues
)

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }
