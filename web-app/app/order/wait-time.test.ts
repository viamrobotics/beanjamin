import { test } from "node:test";
import assert from "node:assert/strict";
import { waitTimeLabel } from "./wait-time";

test("waitTimeLabel hides when the machine sends no estimate", () => {
  assert.equal(waitTimeLabel(undefined), null);
  assert.equal(waitTimeLabel(Number.NaN), null);
});

test("waitTimeLabel says no wait for an idle machine", () => {
  assert.equal(waitTimeLabel(0), "No wait — I'll start right away");
  assert.equal(waitTimeLabel(29), "No wait — I'll start right away");
});

test("waitTimeLabel rounds minutes up", () => {
  assert.equal(waitTimeLabel(30), "About 1 min wait");
  assert.equal(waitTimeLabel(60), "About 1 min wait");
  assert.equal(waitTimeLabel(61), "About 2 min wait");
  assert.equal(waitTimeLabel(310), "About 6 min wait");
});
