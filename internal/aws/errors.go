package aws

import (
	"errors"
	"strings"

	smithy "github.com/aws/smithy-go"
)

// statusMessageMaxLen bounds the length of error text written to a
// metav1.Condition Message. Conditions are surfaced in kubectl describe and UIs
// that typically truncate beyond a few hundred bytes; the value keeps the
// human-readable cause intact while ensuring API server payloads stay small.
const statusMessageMaxLen = 256

// SanitizeError returns a stable, bounded textual representation of err for use
// in metav1.Condition.Message. AWS SDK errors include per-request identifiers
// (RequestID, HostID) that rotate on every retry; writing them into a
// Condition.Message causes the status to change on each attempt, which churns
// status subresource updates and downstream watchers. SanitizeError extracts
// the canonical smithy.APIError code and message when present and substitutes
// it for the transient SDK tail, preserving any context that callers added via
// fmt.Errorf wrapping. Errors without an APIError in the chain are returned
// truncated to statusMessageMaxLen.
func SanitizeError(err error) string {
	if err == nil {
		return ""
	}

	full := err.Error()

	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		full = redactAPIErrorTail(full, apiErr)
	}

	return truncateStatusMessage(full)
}

func redactAPIErrorTail(full string, apiErr smithy.APIError) string {
	canonical := apiErr.ErrorCode() + ": " + apiErr.ErrorMessage()

	tail := apiErr.Error()
	if tail == "" {
		return canonical
	}

	idx := strings.LastIndex(full, tail)
	if idx < 0 {
		return canonical
	}

	return full[:idx] + canonical
}

func truncateStatusMessage(msg string) string {
	if len(msg) <= statusMessageMaxLen {
		return msg
	}

	const ellipsis = "..."

	return msg[:statusMessageMaxLen-len(ellipsis)] + ellipsis
}
