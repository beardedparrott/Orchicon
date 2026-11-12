import { describe, expect, it } from "vitest";

import { MessageDeleteMode } from "@/api/askOrchicon";
import {
  MessageAction,
  confirmationBody,
  confirmationTitle,
  isUndo,
  modeForAction,
  restoresComposer,
} from "@/lib/ask-message-actions";

// THE TWO VERBS DESTROY DIFFERENT AMOUNTS OF HISTORY.
//
// The operator: "click on a user message and undo it (remove it from the screen and context), and also revert to
// that message removing anything that has come after it". Undo takes one row; Revert takes that row and the rest
// of the conversation.
//
// An inverted mapping would be silent and destructive — asking to forget one question would delete everything
// after it. This is the one decision in the UI that can be checked behaviourally rather than grepped, so it is.
describe("the operator's verb maps to the right wire mode", () => {
  it("Undo sends ONLY — the narrow delete", () => {
    expect(modeForAction(MessageAction.Undo)).toBe(MessageDeleteMode.ONLY);
  });

  it("Revert sends AND_FOLLOWING — the truncating delete", () => {
    expect(modeForAction(MessageAction.Revert)).toBe(MessageDeleteMode.AND_FOLLOWING);
  });

  it("the two are distinct, so one cannot stand in for the other", () => {
    expect(modeForAction(MessageAction.Undo)).not.toBe(modeForAction(MessageAction.Revert));
  });

  it("isUndo identifies the narrow verb, for the confirmation copy", () => {
    expect(isUndo(MessageAction.Undo)).toBe(true);
    expect(isUndo(MessageAction.Revert)).toBe(false);
  });
});

// THE CONFIRMATION MUST SAY WHAT SURVIVES, not only what goes.
//
// "Revert" does not tell an operator whether the message they clicked is kept, and the answer differs between
// the verbs: Undo keeps its reply, Revert keeps nothing after it.
describe("the confirmation says exactly how much is removed", () => {
  it("Undo says the reply stays", () => {
    expect(confirmationTitle(MessageAction.Undo)).toBe("Undo this message?");
    expect(confirmationBody(MessageAction.Undo)).toMatch(/reply stays/i);
  });

  it("Revert says everything after it goes", () => {
    expect(confirmationTitle(MessageAction.Revert)).toBe("Revert to this message?");
    expect(confirmationBody(MessageAction.Revert)).toMatch(/everything after it/i);
  });

  it("both say the model's context changes, because that is the half that is invisible", () => {
    for (const action of [MessageAction.Undo, MessageAction.Revert]) {
      expect(confirmationBody(action)).toMatch(/model's context/i);
    }
  });
});

// A REVERT HANDS THE TEXT BACK; AN UNDO DOES NOT.
//
// The operator: "a revert should also put the message you selected back into the composer". Revert removes the
// message, so without the restore its text is gone from the interface entirely and they retype a question they
// had already written. Undo keeps the reply, so restoring the question would leave the transcript reading as
// asked and answered while the composer offered it again.
//
// An inverted answer is silent — the text simply does not appear — which is why the rule is a pure function with
// a test rather than a condition inlined in a component the suite can only grep.
describe("only a revert restores the composer", () => {
  it("revert restores", () => {
    expect(restoresComposer(MessageAction.Revert)).toBe(true);
  });

  it("undo does not", () => {
    expect(restoresComposer(MessageAction.Undo)).toBe(false);
  });

  it("and the two are opposite, so one cannot stand in for the other", () => {
    expect(restoresComposer(MessageAction.Undo)).not.toBe(restoresComposer(MessageAction.Revert));
  });
});
