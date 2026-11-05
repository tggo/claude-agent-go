//go:build integration && unix

package temporal

import (
	"time"

	sdktemporal "go.temporal.io/sdk/temporal"
)

func sdkRetry() sdktemporal.RetryPolicy {
	return sdktemporal.RetryPolicy{MaximumAttempts: 3, InitialInterval: time.Second, BackoffCoefficient: 1}
}
