package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	identityv1 "github.com/appthrust/aws-workload-identity-operator/api/v1alpha1"
)

func TestRequestsForListLogsListFailureAndReturnsNil(t *testing.T) {
	listErr := errors.New("cache list denied")
	entries := []capturedLogEntry{}
	log := logr.New(captureLogSink{entries: &entries}).WithValues(
		"controller", "AWSServiceAccountRole",
		logKeyMapFunc, "rolesForOperatorConfig",
	)

	requests := requestsForList(context.Background(), log, failingListReader{err: listErr}, &identityv1.AWSServiceAccountRoleList{})
	if requests != nil {
		t.Fatalf("expected nil requests on list error, got %#v", requests)
	}

	if len(entries) != 1 {
		t.Fatalf("expected one error log entry, got %d", len(entries))
	}

	entry := entries[0]
	if !errors.Is(entry.err, listErr) {
		t.Fatalf("expected logged list error %v, got %v", listErr, entry.err)
	}

	if entry.msg != "failed to list objects for watch map" {
		t.Fatalf("expected list failure message, got %q", entry.msg)
	}

	assertLogValue(t, entry.values, "controller", "AWSServiceAccountRole")
	assertLogValue(t, entry.values, logKeyMapFunc, "rolesForOperatorConfig")
	assertLogValue(t, entry.values, logKeyOperation, logOpWatchMapList)
	assertLogValue(t, entry.values, logKeyListType, "*v1alpha1.AWSServiceAccountRoleList")
}

func TestRequestsForListLogsExtractFailureAndReturnsNil(t *testing.T) {
	entries := []capturedLogEntry{}
	log := logr.New(captureLogSink{entries: &entries}).WithValues(
		"controller", "AWSWorkloadIdentityConfig",
		logKeyMapFunc, "configsForOperatorConfig",
	)

	requests := requestsForList(context.Background(), log, failingListReader{}, &malformedObjectList{})
	if requests != nil {
		t.Fatalf("expected nil requests on extract error, got %#v", requests)
	}

	if len(entries) != 1 {
		t.Fatalf("expected one error log entry, got %d", len(entries))
	}

	entry := entries[0]
	if entry.err == nil {
		t.Fatal("expected extract error to be logged")
	}

	if entry.msg != "failed to extract objects for watch map" {
		t.Fatalf("expected extract failure message, got %q", entry.msg)
	}

	if !strings.Contains(entry.err.Error(), "Items") {
		t.Fatalf("expected extract error to mention malformed Items field, got %v", entry.err)
	}

	assertLogValue(t, entry.values, "controller", "AWSWorkloadIdentityConfig")
	assertLogValue(t, entry.values, logKeyMapFunc, "configsForOperatorConfig")
	assertLogValue(t, entry.values, logKeyOperation, logOpWatchMapList)
	assertLogValue(t, entry.values, logKeyListType, "*controller.malformedObjectList")
}

type failingListReader struct {
	err error
}

func (r failingListReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return nil
}

func (r failingListReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return r.err
}

type malformedObjectList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items int `json:"items"`
}

func (l *malformedObjectList) DeepCopyObject() kruntime.Object {
	out := *l

	return &out
}

type capturedLogEntry struct {
	err    error
	msg    string
	values []any
}

type captureLogSink struct {
	entries *[]capturedLogEntry
	values  []any
}

func (s captureLogSink) Init(logr.RuntimeInfo) {}

func (s captureLogSink) Enabled(int) bool {
	return true
}

func (s captureLogSink) Info(int, string, ...any) {}

func (s captureLogSink) Error(err error, msg string, keysAndValues ...any) {
	values := append([]any{}, s.values...)
	values = append(values, keysAndValues...)

	*s.entries = append(*s.entries, capturedLogEntry{err: err, msg: msg, values: values})
}

func (s captureLogSink) WithValues(keysAndValues ...any) logr.LogSink {
	values := append([]any{}, s.values...)
	values = append(values, keysAndValues...)

	return captureLogSink{entries: s.entries, values: values}
}

func (s captureLogSink) WithName(string) logr.LogSink {
	return s
}

func assertLogValue(t *testing.T, values []any, key string, want any) {
	t.Helper()

	for i := 0; i+1 < len(values); i += 2 {
		if values[i] == key && values[i+1] == want {
			return
		}
	}

	t.Fatalf("expected log value %q=%v in %#v", key, want, values)
}
