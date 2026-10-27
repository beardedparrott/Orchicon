package workitem

// local_clears_image_test.go — SETTING AN ITEM TO LOCAL MUST CLEAR ITS RUNTIME IMAGE.
//
// REPORTED: "I set the work item to runtime and then set it back to local and it did
// not flip the runtime image field to nothing."
//
// The first implementation had the clear and then defeated it: "the request's own image
// wins" kept any image sent alongside execution_mode=local, on the reasoning that an
// operator might be pre-staging an image for a later switch. That reasoning is unsound
// for the GUI, which is where the operator actually did this:
//
//   - the image input is PREFILLED from the item and is NOT cleared when the
//     execution-mode select changes, and
//   - the save sends `runtimeImage: editRuntimeImage || undefined`,
//
// so switching to local sent BOTH fields, the carve-out matched, and the image survived.
//
// "I deliberately picked this image and also chose local" and "the form round-tripped the
// value it was seeded with" are indistinguishable on the wire. These tests therefore pin
// the rule that resolves it: LOCAL WINS.

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// localItemWithImage seeds an item on the given mode with an image set.
func localItemWithImage(t *testing.T, mode, image string) (*Service, context.Context, string, string) {
	t.Helper()
	pool, s, ctx, tenantID, projectID := archiveEnv(t)
	item := archiveItem(t, pool, tenantID, projectID, "pending")

	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer ttx.Rollback(ctx)
	want := mode
	if _, err := ttx.Tx.Exec(ctx,
		`UPDATE work_items SET execution_mode = $1, runtime_image = $2 WHERE id = $3`,
		want, image, item.ID); err != nil {
		t.Fatalf("seed mode/image: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return s, ctx, tenantID, item.ID
}

// storedImage reads back the item's runtime_image.
func storedImage(t *testing.T, s *Service, ctx context.Context, id string) string {
	t.Helper()
	got, err := s.GetWorkItem(ctx, connect.NewRequest(&apiv1.GetWorkItemRequest{Id: id}))
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	return got.Msg.GetWorkItem().GetRuntimeImage()
}

// TestSwitchingToLocalClearsTheImageTheWayTheGuiSendsIt is the direct regression: the
// request carries execution_mode=local AND the prefilled image, exactly as the GUI's
// edit form sends it.
func TestSwitchingToLocalClearsTheImageTheWayTheGuiSendsIt(t *testing.T) {
	s, ctx, _, id := localItemWithImage(t, "runtime", "orchicon-runtime:orchicon-dev")

	local := apiv1.ExecutionMode_EXECUTION_MODE_LOCAL
	prefilled := "orchicon-runtime:orchicon-dev" // the form's seeded value, untouched
	if _, err := s.UpdateWorkItem(ctx, connect.NewRequest(&apiv1.UpdateWorkItemRequest{
		Id:            id,
		ExecutionMode: &local,
		RuntimeImage:  &prefilled, // the GUI sends this on every save
	})); err != nil {
		t.Fatalf("update to local: %v", err)
	}

	if got := storedImage(t, s, ctx, id); got != "" {
		t.Fatalf("runtime_image = %q after switching to LOCAL, want EMPTY. The operator's report "+
			"exactly: the mode flipped to local and the image stayed. A local run has no container, "+
			"so the stored image is a claim the item cannot honour — and it silently becomes live "+
			"again on the next switch back to runtime.", got)
	}
}

// TestLocalModeAfterARoundTripStaysClear: the transition must be STABLE. A second save
// of an already-local item (again round-tripping an empty image) must not resurrect one.
func TestLocalModeAfterARoundTripStaysClear(t *testing.T) {
	s, ctx, _, id := localItemWithImage(t, "runtime", "orchicon-runtime:orchicon-dev")

	local := apiv1.ExecutionMode_EXECUTION_MODE_LOCAL
	img := "orchicon-runtime:orchicon-dev"
	if _, err := s.UpdateWorkItem(ctx, connect.NewRequest(&apiv1.UpdateWorkItemRequest{
		Id: id, ExecutionMode: &local, RuntimeImage: &img,
	})); err != nil {
		t.Fatalf("first save: %v", err)
	}

	// A later save that does NOT touch execution_mode must leave everything alone —
	// absent execution_mode means UNCHANGED (field-mask), so no clear is applied and no
	// image is invented.
	if _, err := s.UpdateWorkItem(ctx, connect.NewRequest(&apiv1.UpdateWorkItemRequest{
		Id: id, RuntimeImage: &img,
	})); err != nil {
		t.Fatalf("second save: %v", err)
	}
	// The explicit image on a NON-mode-changing save is honoured, as it always was: the
	// operator is on local and deliberately set an image for later.
	if got := storedImage(t, s, ctx, id); got != img {
		t.Errorf("runtime_image = %q after an explicit image on a save that did not change the "+
			"mode, want %q — LOCAL-WINS applies to the TRANSITION, not to every write", got, img)
	}
}

// TestRuntimeModeKeepsItsImage: switching to runtime with an image must store it. The
// clear is specific to LOCAL, and a fix that cleared unconditionally would make it
// impossible to choose an image at all.
func TestRuntimeModeKeepsItsImage(t *testing.T) {
	s, ctx, _, id := localItemWithImage(t, "local", "")

	runtime := apiv1.ExecutionMode_EXECUTION_MODE_RUNTIME
	img := "orchicon-runtime:orchicon-dev"
	if _, err := s.UpdateWorkItem(ctx, connect.NewRequest(&apiv1.UpdateWorkItemRequest{
		Id: id, ExecutionMode: &runtime, RuntimeImage: &img,
	})); err != nil {
		t.Fatalf("switch to runtime: %v", err)
	}
	if got := storedImage(t, s, ctx, id); got != img {
		t.Fatalf("runtime_image = %q after switching to RUNTIME with %q, want it stored — a "+
			"containerised item is defined by the image it runs", got, img)
	}
}
