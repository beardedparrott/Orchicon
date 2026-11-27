package askorchicon

import (
	"encoding/json"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/workitem"
)

// WHY THIS FILE EXISTS.
//
// The operator creates work through Ask (Brainstorm / Iteration / Quick Work). Work that needs a
// credential — a dev-plane token, a provider key — has to have it attached at CREATION, because an
// item that reaches dispatch without the credential it needs fails at run time with no obvious
// cause. The tool had no `secret_ids` field at all, so an agent could not attach one and the operator
// had to remember to add it afterwards, every time. Their words:
//
//	"add secrets to the list of questions that brainstorm, iteration, and quick work ask you if it is
//	 creating work items for you and gives you the list to choose from. That way I don't forget."
//
// So the schema must OFFER the field (an agent cannot ask about a field that does not exist), and the
// handler must ACCEPT and VALIDATE it.

func createWorkItemTool(t *testing.T) ToolDefinition {
	t.Helper()
	reg := NewToolRegistry(nil, slog.Default(), nil)
	for _, def := range reg.List() {
		if def.Name == "create_work_item" {
			return def
		}
	}
	t.Fatal("create_work_item is not registered")
	return ToolDefinition{}
}

// TestCreateWorkItemSchemaOffersSecretIDs — the field must exist for the agent to ask about it.
func TestCreateWorkItemSchemaOffersSecretIDs(t *testing.T) {
	def := createWorkItemTool(t)

	prop, ok := def.Properties["secret_ids"]
	if !ok {
		t.Fatal("create_work_item has no `secret_ids` property. An agent cannot ask the operator which " +
			"secrets a piece of work needs if the field does not exist, so the credential is attached " +
			"late or not at all.")
	}
	if prop.Type != "array" {
		t.Errorf("secret_ids type = %q, want array", prop.Type)
	}
	// The description must TELL the agent to ask, not merely permit the field.
	if !strings.Contains(strings.ToUpper(prop.Description), "ALWAYS ASK") {
		t.Errorf("secret_ids description does not instruct the agent to ASK the operator:\n%s", prop.Description)
	}
	if prop.Items == nil {
		t.Error("secret_ids is an array with no `items` — Gemini rejects the whole schema with 400")
	}
}

// TestCreateWorkItemDescriptionNamesSecretIDs — the one-line summary lists the field.
func TestCreateWorkItemDescriptionNamesSecretIDs(t *testing.T) {
	def := createWorkItemTool(t)
	if !strings.Contains(def.Description, "secret_ids") {
		t.Error("create_work_item's description does not name secret_ids, so an agent skimming it has " +
			"no reason to look")
	}
}

// TestCreateWorkItemAcceptsSecretIDs — the handler must have somewhere to put them.
func TestCreateWorkItemAcceptsSecretIDs(t *testing.T) {
	var params struct {
		SecretIds []string `json:"secret_ids"`
	}
	raw := json.RawMessage(`{"title":"t","project_id":"p","secret_ids":["a","b"]}`)
	if err := json.Unmarshal(raw, &params); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(params.SecretIds, []string{"a", "b"}) {
		t.Fatalf("secret_ids did not bind: %+v", params.SecretIds)
	}
}

// TestCreateWorkItemValidatesSecretIDs — the same rule the RPC applies, so an agent cannot create an
// item the UI would refuse, and the allow-list behind `orchicon secret get` keeps one shape.
func TestCreateWorkItemValidatesSecretIDs(t *testing.T) {
	cases := map[string][]string{
		"more than ten": {"1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11"},
		"empty element": {"a", ""},
		"duplicate id":  {"a", "a"},
		"single, valid": {"a"},
	}
	for name, ids := range cases {
		err := workitem.ValidateSecretIDs(ids)
		if name == "single, valid" {
			if err != nil {
				t.Errorf("%s: unexpected error %v", name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: expected a validation error for %v", name, ids)
		}
	}
}
