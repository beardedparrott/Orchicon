package opencode

// apisurface_modelfield_test.go — THE FIELD INSIDE `model` MUST MATCH THE ROUTE THAT CARRIES IT.
//
// REPORTED from a macOS box running opencode 1.19.5 (Linux and Windows were on 2.x, where it worked):
//
//	conversation session send: opencode session set model: opencode serve POST
//	/api/session/ses_…/model: http 400: {"_tag":"InvalidRequestError",
//	  "message":"Missing key\n  at [\"model\"][\"id\"]","kind":"Payload"}
//
// The model could never be set, so every 1.18+ turn died before a token was sent. Reproduced against a real
// 1.19.5 serve, both bodies probed directly:
//
//	{"model":{"providerID":…,"modelID":…}}   -> 400  Missing key at ["model"]["id"]     ← what v1+api-prompt sent
//	{"model":{"providerID":…,"id":…}}        -> 204                                     ← what it needs
//
// THE CAUSE IS A SHAPE THAT BORROWS FROM TWO VERSIONS. `v1+api-prompt` keeps v1's routes but sets the model on
// v2's `/api/session/{id}/model`, and the two disagree about the key's name: v1's `prompt_async` carries
// `{providerID, modelID}`, v2's Model.Ref carries `{providerID, id}`. The shape overrode the ROUTE and inherited
// the FIELD, so it sent v1's spelling to v2's route — a mismatch that no version comparison could catch, because
// both halves were individually correct.
//
// Nothing on 2.x saw it: shapeV2 sets both from the same table entry. Only the borrowed shape could be wrong.

import "testing"

// EVERY SHAPE THAT SETS THE MODEL ON A SESSION MUST NAME THE KEY THAT ROUTE EXPECTS.
//
// Driven from the table rather than by naming one shape: a future shape that borrows a route inherits this
// assertion instead of inheriting the bug.
func TestTheModelFieldMatchesTheRouteThatCarriesIt(t *testing.T) {
	cases := []struct {
		shape apiShape
		want  string
		why   string
	}{
		{
			shape: shapeV2, want: "id",
			why: "v2 sets the model on /api/session/{id}/model, whose Model.Ref is {id, providerID}",
		},
		{
			shape: shapeV1APIPrompt, want: "id",
			why: "this shape borrows the SAME /api/.../model route to set the model — the measured 400 " +
				"`Missing key at [\"model\"][\"id\"]` is what v1's `modelID` spelling earns there",
		},
		{
			shape: shapeV1, want: "modelID",
			why: "v1 carries the model in the prompt_async body, whose own spelling is `modelID` — a different " +
				"route with a different contract, which is why only the borrowed shape moved",
		},
	}
	for _, tc := range cases {
		if tc.shape.modelFieldName != tc.want {
			t.Errorf("shape %q: modelFieldName = %q, want %q — %s",
				tc.shape.name, tc.shape.modelFieldName, tc.want, tc.why)
		}
	}
}

// AND THE SHAPE A 1.19 SERVE ACTUALLY RESOLVES TO IS THE ONE THAT WAS BROKEN. This is the reported version,
// asserted at the entry point rather than at the table, so a regression in version routing fails here too.
func TestTheReportedVersionResolvesToACorrectModelField(t *testing.T) {
	for _, version := range []string{"1.18.34", "1.19.4", "1.19.5"} {
		sh, parsed := apiShapeForVersion(version)
		if !parsed {
			t.Fatalf("version %q did not parse", version)
		}
		if sh.setModelPath == nil {
			// A shape that carries the model in the prompt body uses v1's spelling on v1's route.
			if sh.modelFieldName != "modelID" {
				t.Errorf("version %q -> shape %q carries the model in its prompt body but names it %q, want "+
					"modelID", version, sh.name, sh.modelFieldName)
			}
			continue
		}
		if sh.modelFieldName != "id" {
			t.Errorf("version %q -> shape %q sets the model on %s, which requires %q, but sends %q — that is "+
				"the 400 the operator hit on macOS", version, sh.name, sh.setModelPath("ses_x"), "id",
				sh.modelFieldName)
		}
	}
}
