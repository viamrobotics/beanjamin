import { test } from "node:test";
import assert from "node:assert/strict";
import {
  computeReliability,
  groupByRobot,
  UNKNOWN_STEP,
  type OrderOutcome,
} from "./reliability";

const DAY = 24 * 60 * 60 * 1000;
const T0 = new Date("2026-11-01T12:00:00Z").getTime();

// One outcome per minute from T0; "s" succeeds, "f" fails at step "Brewing".
function outcomes(pattern: string, startMs = T0): OrderOutcome[] {
  return [...pattern].map((c, i) => ({
    robotId: "r1",
    at: new Date(startMs + i * 60_000),
    ok: c === "s",
    failedStep: c === "s" ? "" : "Brewing",
  }));
}

const before = new Date(T0 - DAY);

test("consecutive failures are one incident", () => {
  const r = computeReliability("r1", outcomes("sssfffsssss"), before);
  assert.equal(r.incidents.length, 1);
  assert.equal(r.failures, 3);
  assert.equal(r.drinks, 8);
  assert.equal(r.mdbf, 8);
  assert.equal(r.drinksSinceLastIncident, 5);
  assert.equal(r.sinceLastIsLowerBound, false);
});

test("a success between failures splits them into two incidents", () => {
  const r = computeReliability("r1", outcomes("ssfsssfs"), before);
  assert.equal(r.incidents.length, 2);
  assert.equal(r.failures, 2);
  assert.equal(r.mdbf, 6 / 2);
  assert.deepEqual(
    r.incidents.map((i) => i.drinksBefore),
    [2, 3]
  );
  assert.deepEqual(
    r.incidents.map((i) => i.drinksBeforeIsLowerBound),
    [true, false]
  );
});

test("an incident still open at the end counts", () => {
  const r = computeReliability("r1", outcomes("ssssff"), before);
  assert.equal(r.incidents.length, 1);
  assert.equal(r.failures, 2);
  assert.equal(r.drinksSinceLastIncident, 0);
});

test("no incident leaves mdbf null and the streak a lower bound", () => {
  const r = computeReliability("r1", outcomes("sssss"), before);
  assert.equal(r.mdbf, null);
  assert.equal(r.drinks, 5);
  assert.equal(r.drinksSinceLastIncident, 5);
  assert.equal(r.sinceLastIsLowerBound, true);
});

// An incident that began before the window belongs to the earlier period, even
// when its retries spill into the window.
test("only incidents starting in the window count", () => {
  const all = outcomes("ssffffsss");
  const windowStart = all[3].at;
  const r = computeReliability("r1", all, windowStart);
  assert.equal(r.incidents.length, 0);
  assert.equal(r.drinks, 3);
  assert.equal(r.drinksSinceLastIncident, 3);
  assert.equal(r.sinceLastIsLowerBound, false);
});

test("drinks before the window are not counted in it", () => {
  const all = outcomes("sssssssfss");
  const r = computeReliability("r1", all, all[5].at);
  assert.equal(r.drinks, 4);
  assert.equal(r.incidents[0].drinksBefore, 7);
});

test("top steps rank incidents by their first failure's step", () => {
  const list: OrderOutcome[] = [
    { robotId: "r1", at: new Date(T0), ok: false, failedStep: "Grinding" },
    { robotId: "r1", at: new Date(T0 + 1), ok: false, failedStep: "Brewing" },
    { robotId: "r1", at: new Date(T0 + 2), ok: true, failedStep: "" },
    { robotId: "r1", at: new Date(T0 + 3), ok: false, failedStep: "Brewing" },
    { robotId: "r1", at: new Date(T0 + 4), ok: true, failedStep: "" },
    { robotId: "r1", at: new Date(T0 + 5), ok: false, failedStep: "Brewing" },
    { robotId: "r1", at: new Date(T0 + 6), ok: true, failedStep: "" },
    { robotId: "r1", at: new Date(T0 + 7), ok: false, failedStep: "" },
  ];
  const r = computeReliability("r1", list, before);
  assert.deepEqual(r.topSteps, [
    { step: "Brewing", count: 2 },
    { step: "Grinding", count: 1 },
    { step: UNKNOWN_STEP, count: 1 },
  ]);
});

test("groupByRobot keeps each machine's order", () => {
  const a = outcomes("sf");
  const b = outcomes("fs").map((o) => ({ ...o, robotId: "r2" }));
  const grouped = groupByRobot([a[0], b[0], a[1], b[1]]);
  assert.deepEqual(grouped.get("r1"), a);
  assert.deepEqual(grouped.get("r2"), b);
});
