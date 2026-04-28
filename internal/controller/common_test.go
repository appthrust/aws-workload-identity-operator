package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

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

// capturedInfoLogEntry / captureInfoLogSink mirror captureLogSink but for
// Info-level emissions. It is kept separate from captureLogSink so existing
// Error-only tests don't have their entries[0] index shifted by Info noise from
// the code under test.
type capturedInfoLogEntry struct {
	msg    string
	values []any
}

type captureInfoLogSink struct {
	entries *[]capturedInfoLogEntry
	values  []any
}

func (s captureInfoLogSink) Init(logr.RuntimeInfo) {}

func (s captureInfoLogSink) Enabled(int) bool {
	return true
}

func (s captureInfoLogSink) Info(_ int, msg string, keysAndValues ...any) {
	values := append([]any{}, s.values...)
	values = append(values, keysAndValues...)

	*s.entries = append(*s.entries, capturedInfoLogEntry{msg: msg, values: values})
}

func (s captureInfoLogSink) Error(error, string, ...any) {}

func (s captureInfoLogSink) WithValues(keysAndValues ...any) logr.LogSink {
	values := append([]any{}, s.values...)
	values = append(values, keysAndValues...)

	return captureInfoLogSink{entries: s.entries, values: values}
}

func (s captureInfoLogSink) WithName(string) logr.LogSink {
	return s
}

// finalizerWireCounter counts Patch vs Update calls reaching the fake client;
// finalizer mutations must use Patch so they don't blow away concurrent writes.
type finalizerWireCounter struct {
	patches int
	updates int
}

func (w *finalizerWireCounter) interceptorFuncs() interceptor.Funcs {
	return interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			w.patches++

			return c.Patch(ctx, obj, patch, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			w.updates++

			return c.Update(ctx, obj, opts...)
		},
	}
}

func newFinalizerTestClient(t *testing.T, role *identityv1.AWSServiceAccountRole, w *finalizerWireCounter) client.Client {
	t.Helper()

	return fake.NewClientBuilder().
		WithScheme(testControllerScheme(t)).
		WithObjects(role).
		WithInterceptorFuncs(w.interceptorFuncs()).
		Build()
}

// The MergeFrom snapshot must be taken before AddFinalizer so a caller-side
// spec mutation after Get does not leak into the on-server state via the patch.
func TestEnsureFinalizerUsesPatchAndIsolatesCallerMutations(t *testing.T) {
	role := &identityv1.AWSServiceAccountRole{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
		Spec: identityv1.AWSServiceAccountRoleSpec{
			ServiceAccount: identityv1.ServiceAccountSubject{Namespace: "default", Name: "app"},
			PolicyARNs:     []string{"arn:aws:iam::aws:policy/ReadOnlyAccess"},
		},
	}

	wire := &finalizerWireCounter{}
	c := newFinalizerTestClient(t, role, wire)
	recorder := &capturingEventRecorder{}

	working := &identityv1.AWSServiceAccountRole{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(role), working); err != nil {
		t.Fatalf("seed get: %v", err)
	}

	// Simulate a caller that mutated the in-memory spec after Get and before
	// invoking ensureFinalizer. With a correct MergeFrom snapshot taken inside
	// ensureFinalizer, this rogue change must NOT be persisted by the patch
	// (the patch should only carry the finalizer delta).
	working.Spec.PolicyARNs = []string{"arn:aws:iam::aws:policy/AdministratorAccess"}

	added, err := ensureFinalizer(context.Background(), c, recorder, logr.Discard(), working, identityv1.ServiceAccountRoleFinalizer)
	if err != nil {
		t.Fatalf("ensureFinalizer: %v", err)
	}

	if !added {
		t.Fatal("expected ensureFinalizer to report finalizer added")
	}

	if wire.patches != 1 || wire.updates != 0 {
		t.Fatalf("expected exactly 1 Patch and 0 Updates, got patches=%d updates=%d", wire.patches, wire.updates)
	}

	assertRecordedEvents(t, recorder.events, []recordedEvent{{
		regarding: working,
		eventType: corev1.EventTypeNormal,
		reason:    eventReasonFinalizerAdded,
		action:    eventActionAddFinalizer,
		note:      "added finalizer for cleanup",
	}})

	stored := &identityv1.AWSServiceAccountRole{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(role), stored); err != nil {
		t.Fatalf("post-patch get: %v", err)
	}

	if !controllerutil.ContainsFinalizer(stored, identityv1.ServiceAccountRoleFinalizer) {
		t.Fatalf("expected finalizer %q on stored object, got %#v", identityv1.ServiceAccountRoleFinalizer, stored.Finalizers)
	}

	// The caller-side spec mutation must NOT have leaked through the patch:
	// the on-server PolicyARNs should still be the original ReadOnlyAccess
	// because the MergeFrom base captured pre-mutation state.
	if len(stored.Spec.PolicyARNs) != 1 || stored.Spec.PolicyARNs[0] != "arn:aws:iam::aws:policy/ReadOnlyAccess" {
		t.Fatalf("caller-side spec mutation leaked through finalizer patch; expected PolicyARNs=[ReadOnlyAccess], got %#v", stored.Spec.PolicyARNs)
	}
}

func TestEnsureFinalizerNoOpDoesNotPatchOrUpdate(t *testing.T) {
	role := &identityv1.AWSServiceAccountRole{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "app",
			Namespace:  "default",
			Finalizers: []string{identityv1.ServiceAccountRoleFinalizer},
		},
	}

	wire := &finalizerWireCounter{}
	c := newFinalizerTestClient(t, role, wire)
	recorder := &capturingEventRecorder{}

	working := &identityv1.AWSServiceAccountRole{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(role), working); err != nil {
		t.Fatalf("seed get: %v", err)
	}

	added, err := ensureFinalizer(context.Background(), c, recorder, logr.Discard(), working, identityv1.ServiceAccountRoleFinalizer)
	if err != nil {
		t.Fatalf("ensureFinalizer: %v", err)
	}

	if added {
		t.Fatal("expected ensureFinalizer to report no-op when finalizer already present")
	}

	if wire.patches != 0 || wire.updates != 0 {
		t.Fatalf("expected no API writes for no-op ensureFinalizer, got patches=%d updates=%d", wire.patches, wire.updates)
	}

	if len(recorder.events) != 0 {
		t.Fatalf("expected no events on no-op ensureFinalizer, got %#v", recorder.events)
	}
}

// Update would blow away unrelated fields when a writer races between the
// controller's Get and Update; finalizer release must use Patch instead.
func TestRemoveFinalizerUsesPatch(t *testing.T) {
	role := &identityv1.AWSServiceAccountRole{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "app",
			Namespace:  "default",
			Finalizers: []string{identityv1.ServiceAccountRoleFinalizer},
		},
	}

	wire := &finalizerWireCounter{}
	c := newFinalizerTestClient(t, role, wire)
	recorder := &capturingEventRecorder{}

	working := &identityv1.AWSServiceAccountRole{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(role), working); err != nil {
		t.Fatalf("seed get: %v", err)
	}

	if err := removeFinalizer(context.Background(), c, recorder, logr.Discard(), working, identityv1.ServiceAccountRoleFinalizer); err != nil {
		t.Fatalf("removeFinalizer: %v", err)
	}

	if wire.patches != 1 || wire.updates != 0 {
		t.Fatalf("expected exactly 1 Patch and 0 Updates, got patches=%d updates=%d", wire.patches, wire.updates)
	}

	assertRecordedEvents(t, recorder.events, []recordedEvent{{
		regarding: working,
		eventType: corev1.EventTypeNormal,
		reason:    eventReasonFinalizerRemoved,
		action:    eventActionRemoveFinalizer,
		note:      "removed finalizer after cleanup",
	}})

	stored := &identityv1.AWSServiceAccountRole{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(role), stored); err != nil {
		t.Fatalf("post-patch get: %v", err)
	}

	if controllerutil.ContainsFinalizer(stored, identityv1.ServiceAccountRoleFinalizer) {
		t.Fatalf("expected finalizer %q removed, got %#v", identityv1.ServiceAccountRoleFinalizer, stored.Finalizers)
	}
}

func TestRemoveFinalizerNoOpDoesNotPatchOrUpdate(t *testing.T) {
	role := &identityv1.AWSServiceAccountRole{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
	}

	wire := &finalizerWireCounter{}
	c := newFinalizerTestClient(t, role, wire)
	recorder := &capturingEventRecorder{}

	working := &identityv1.AWSServiceAccountRole{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(role), working); err != nil {
		t.Fatalf("seed get: %v", err)
	}

	if err := removeFinalizer(context.Background(), c, recorder, logr.Discard(), working, identityv1.ServiceAccountRoleFinalizer); err != nil {
		t.Fatalf("removeFinalizer: %v", err)
	}

	if wire.patches != 0 || wire.updates != 0 {
		t.Fatalf("expected no API writes for no-op removeFinalizer, got patches=%d updates=%d", wire.patches, wire.updates)
	}

	if len(recorder.events) != 0 {
		t.Fatalf("expected no events on no-op removeFinalizer, got %#v", recorder.events)
	}
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
