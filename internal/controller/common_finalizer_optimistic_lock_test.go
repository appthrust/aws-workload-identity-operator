package controller

import (
	"context"
	"slices"
	"testing"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	identityv1 "github.com/appthrust/aws-workload-identity-operator/api/v1alpha1"
)

// foreignFinalizer simulates a finalizer owned by an unrelated actor; if the
// finalizer-mutating patches are not optimistic-locked, the finalizer-array
// merge would clobber it whenever the on-server resourceVersion advances
// between our Get and our Patch.
const foreignFinalizer = "foreign.example.io/protect"

// raceWithForeignWriter sets up the fake-client race scenario shared by both
// regression tests: it Gets the seeded role into `working` (the caller's stale
// snapshot), then has a separate "foreign" actor Get-and-Patch the on-server
// object to add foreignFinalizer (advancing its resourceVersion). It returns
// the seed object's key plus the caller's stale `working` copy.
func raceWithForeignWriter(t *testing.T, c client.Client, role *identityv1.AWSServiceAccountRole) (client.ObjectKey, *identityv1.AWSServiceAccountRole) {
	t.Helper()

	key := client.ObjectKeyFromObject(role)

	working := &identityv1.AWSServiceAccountRole{}
	if err := c.Get(context.Background(), key, working); err != nil {
		t.Fatalf("seed get: %v", err)
	}

	foreign := &identityv1.AWSServiceAccountRole{}
	if err := c.Get(context.Background(), key, foreign); err != nil {
		t.Fatalf("foreign get: %v", err)
	}

	foreignBase := foreign.DeepCopy()
	foreign.Finalizers = append(foreign.Finalizers, foreignFinalizer)

	if err := c.Patch(context.Background(), foreign, client.MergeFrom(foreignBase)); err != nil {
		t.Fatalf("foreign patch: %v", err)
	}

	if working.ResourceVersion == foreign.ResourceVersion {
		t.Fatalf("foreign patch failed to advance resourceVersion (still %q); test cannot exercise the race",
			working.ResourceVersion)
	}

	return key, working
}

func newOptimisticLockTestClient(t *testing.T, role *identityv1.AWSServiceAccountRole) client.Client {
	t.Helper()

	return fake.NewClientBuilder().
		WithScheme(testControllerScheme(t)).
		WithObjects(role).
		Build()
}

// TestEnsureFinalizerReturnsConflictOnRaceWithForeignWriter exercises the
// optimistic-lock guard in ensureFinalizer: when another actor advances the
// resourceVersion (here by adding an unrelated finalizer) between our Get and
// our Patch, the Patch must fail with apierrors.IsConflict so the controller
// re-queues, and the foreign finalizer must remain intact on the server. With
// plain MergeFrom (no OptimisticLock) the finalizer-array merge would silently
// overwrite finalizers to [ourFinalizer], dropping the foreign one — that is
// the regression this test pins.
func TestEnsureFinalizerReturnsConflictOnRaceWithForeignWriter(t *testing.T) {
	role := &identityv1.AWSServiceAccountRole{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
		Spec: identityv1.AWSServiceAccountRoleSpec{
			ServiceAccount: identityv1.ServiceAccountSubject{Namespace: "default", Name: "app"},
			PolicyARNs:     []string{"arn:aws:iam::aws:policy/ReadOnlyAccess"},
		},
	}

	c := newOptimisticLockTestClient(t, role)
	key, working := raceWithForeignWriter(t, c, role)
	recorder := &capturingEventRecorder{}

	added, err := ensureFinalizer(context.Background(), c, recorder, logr.Discard(), working, identityv1.ServiceAccountRoleFinalizer)
	if err == nil {
		t.Fatalf("expected ensureFinalizer to fail with Conflict, got nil error (added=%v)", added)
	}

	if !apierrors.IsConflict(err) {
		t.Fatalf("expected apierrors.IsConflict(err)==true, got %v", err)
	}

	if added {
		t.Fatalf("expected added=false on conflict, got true")
	}

	// The on-server state must still carry the foreign finalizer, and must NOT
	// have been clobbered to [ourFinalizer] by a non-optimistic merge patch.
	stored := &identityv1.AWSServiceAccountRole{}
	if err := c.Get(context.Background(), key, stored); err != nil {
		t.Fatalf("post-conflict get: %v", err)
	}

	if !slices.Contains(stored.Finalizers, foreignFinalizer) {
		t.Fatalf("foreign finalizer was clobbered by non-optimistic patch; finalizers=%#v", stored.Finalizers)
	}

	if slices.Contains(stored.Finalizers, identityv1.ServiceAccountRoleFinalizer) {
		t.Fatalf("our finalizer must not have been persisted on conflict; finalizers=%#v", stored.Finalizers)
	}

	if len(recorder.events) != 0 {
		t.Fatalf("no FinalizerAdded event must be recorded on conflict; got %#v", recorder.events)
	}
}

// TestRemoveFinalizerReturnsConflictOnRaceWithForeignWriter is the symmetric
// case for removeFinalizer: another actor adds an unrelated finalizer between
// our Get and our Patch. With OptimisticLock the Patch fails Conflict and the
// foreign finalizer is preserved; with plain MergeFrom the finalizer array
// would be merge-overwritten to [] (because our base contained [ourFinalizer]
// and our modified contained []), silently dropping the foreign finalizer.
func TestRemoveFinalizerReturnsConflictOnRaceWithForeignWriter(t *testing.T) {
	role := &identityv1.AWSServiceAccountRole{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "app",
			Namespace:  "default",
			Finalizers: []string{identityv1.ServiceAccountRoleFinalizer},
		},
	}

	c := newOptimisticLockTestClient(t, role)
	key, working := raceWithForeignWriter(t, c, role)
	recorder := &capturingEventRecorder{}

	err := removeFinalizer(context.Background(), c, recorder, logr.Discard(), working, identityv1.ServiceAccountRoleFinalizer)
	if err == nil {
		t.Fatal("expected removeFinalizer to fail with Conflict, got nil error")
	}

	if !apierrors.IsConflict(err) {
		t.Fatalf("expected apierrors.IsConflict(err)==true, got %v", err)
	}

	stored := &identityv1.AWSServiceAccountRole{}
	if err := c.Get(context.Background(), key, stored); err != nil {
		t.Fatalf("post-conflict get: %v", err)
	}

	if !slices.Contains(stored.Finalizers, foreignFinalizer) {
		t.Fatalf("foreign finalizer was clobbered by non-optimistic patch; finalizers=%#v", stored.Finalizers)
	}

	if !slices.Contains(stored.Finalizers, identityv1.ServiceAccountRoleFinalizer) {
		t.Fatalf("our finalizer must remain on conflict (no successful removal); finalizers=%#v", stored.Finalizers)
	}

	if len(recorder.events) != 0 {
		t.Fatalf("no FinalizerRemoved event must be recorded on conflict; got %#v", recorder.events)
	}
}
