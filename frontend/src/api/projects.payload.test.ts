// THE UPDATE PAYLOAD MUST BE ENCODABLE, NOT JUST ASSIGNABLE.
//
// REPORTED: saving a project's skill files failed with
//
//   cannot encode field orchicon.api.v1.UpdateProjectRequest.skill_files to JSON:
//   bte(...).toJson is not a function
//
// This is protobuf-es v1: `new UpdateProjectRequest({...})` returns a Message, but a message-typed field
// assigned AFTER construction is not converted — it stays whatever was assigned. Connect serialises the
// request by calling toJson() on each message member, so a plain object literal has no toJson and the
// request never leaves the browser. Passing the same object literal to the CLIENT works (the client runs
// initPartial over it), which is why every other update path looked fine.
//
// These assertions encode the payload the way the client does, so an assignment that is merely
// type-castable but not encodable fails here instead of in the operator's face.

import { describe, expect, it } from "vitest";

import { buildUpdateProjectRequest } from "@/api/projects";

/** encode mirrors what Connect does to a request: the JSON of the message, member by member. */
function encode(payload: { toJson: () => unknown }) {
  return payload.toJson() as Record<string, unknown>;
}

describe("the project update payload is encodable", () => {
  it("skill files survive serialisation", () => {
    const payload = buildUpdateProjectRequest({ id: "p1", skillFiles: ["/work/skills/a.md", "/work/skills"] });
    const json = encode(payload) as { skillFiles?: { files?: string[] } };
    expect(json.skillFiles?.files).toEqual(["/work/skills/a.md", "/work/skills"]);
  });

  it("an empty skill-file list is sent, so it can CLEAR the selection", () => {
    const payload = buildUpdateProjectRequest({ id: "p1", skillFiles: [] });
    expect((encode(payload) as { skillFiles?: { files?: string[] } }).skillFiles).toBeDefined();
  });

  it("an absent skill-file list is NOT sent (field-mask semantics)", () => {
    const payload = buildUpdateProjectRequest({ id: "p1" });
    expect((encode(payload) as { skillFiles?: unknown }).skillFiles).toBeUndefined();
  });

  it("goals survive serialisation", () => {
    const payload = buildUpdateProjectRequest({
      id: "p1",
      goals: { fields: [{ key: "Goal", value: "Ship it" } as never] },
    });
    const json = encode(payload) as { goals?: { fields?: { key?: string; value?: string }[] } };
    expect(json.goals?.fields?.[0]).toMatchObject({ key: "Goal", value: "Ship it" });
  });
});
