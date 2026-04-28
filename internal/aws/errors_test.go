package aws

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	smithy "github.com/aws/smithy-go"
)

// fakeAPIError mirrors the shape of an AWS SDK SmithyAPIError. The SDK appends
// per-request identifiers (RequestID, HostID) into Error(), and those rotate on
// every retry — the exact case SanitizeError must redact from condition
// messages.
type fakeAPIError struct {
	code    string
	message string
	fault   smithy.ErrorFault
	tail    string
}

func (e *fakeAPIError) Error() string {
	base := e.code + ": " + e.message
	if e.tail != "" {
		return base + " " + e.tail
	}

	return base
}

func (e *fakeAPIError) ErrorCode() string             { return e.code }
func (e *fakeAPIError) ErrorMessage() string          { return e.message }
func (e *fakeAPIError) ErrorFault() smithy.ErrorFault { return e.fault }

func TestSanitizeErrorNil(t *testing.T) {
	if got := SanitizeError(nil); got != "" {
		t.Fatalf("expected empty string for nil error, got %q", got)
	}
}

func TestSanitizeErrorStripsTransientSDKDetail(t *testing.T) {
	// First attempt and retry produce different RequestID/HostID tails; the
	// sanitized message must be byte-identical so Condition.Message does not
	// thrash on every reconcile.
	first := &fakeAPIError{
		code:    "AccessDenied",
		message: "Access Denied",
		tail:    "RequestID: REQ-001, HostID: HOST-001",
	}
	retry := &fakeAPIError{
		code:    "AccessDenied",
		message: "Access Denied",
		tail:    "RequestID: REQ-002, HostID: HOST-002",
	}

	wrap := func(apiErr error) error {
		return fmt.Errorf("publish self-hosted OIDC issuer objects: %w",
			fmt.Errorf("put S3 object s3://bucket/key: %w", apiErr))
	}

	firstMsg := SanitizeError(wrap(first))
	retryMsg := SanitizeError(wrap(retry))

	if firstMsg != retryMsg {
		t.Fatalf("expected stable message across retries; first=%q retry=%q", firstMsg, retryMsg)
	}

	for _, leaked := range []string{"REQ-001", "REQ-002", "HOST-001", "HOST-002", "RequestID", "HostID"} {
		if strings.Contains(firstMsg, leaked) {
			t.Fatalf("sanitized message %q must not contain transient SDK detail %q", firstMsg, leaked)
		}
	}

	wantPrefix := "publish self-hosted OIDC issuer objects: put S3 object s3://bucket/key: "
	if !strings.HasPrefix(firstMsg, wantPrefix) {
		t.Fatalf("sanitized message %q must preserve caller wrapping prefix %q", firstMsg, wantPrefix)
	}

	if !strings.HasSuffix(firstMsg, "AccessDenied: Access Denied") {
		t.Fatalf("sanitized message %q must end with canonical code: message", firstMsg)
	}
}

func TestSanitizeErrorPlainErrorTruncated(t *testing.T) {
	long := strings.Repeat("x", statusMessageMaxLen+50)
	got := SanitizeError(errors.New(long))

	if len(got) != statusMessageMaxLen {
		t.Fatalf("expected truncation to %d bytes, got %d (%q)", statusMessageMaxLen, len(got), got)
	}

	if !strings.HasSuffix(got, "...") {
		t.Fatalf("expected truncation suffix \"...\", got %q", got)
	}
}

func TestSanitizeErrorPlainErrorBelowLimit(t *testing.T) {
	plain := errors.New("bucket is empty")
	if got := SanitizeError(plain); got != "bucket is empty" {
		t.Fatalf("expected pass-through for short non-API error, got %q", got)
	}
}
