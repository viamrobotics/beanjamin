import { test } from "node:test";
import assert from "node:assert/strict";
import * as VIAM from "@viamrobotics/sdk";
import {
  buildOrderFileFilter,
  startOfToday,
  panelKey,
  panelTitle,
  planTimeLabel,
  unslugStep,
  VIDEO_MIME,
  PLAN_MIME,
} from "./data";

// The order ID tags both the clips and every motion-plan JSON (one file per
// planned motion). Only the mime type tells them apart — that is how the clip
// list filled with plan files that rendered as broken video players.
test("the mime type is what separates clips from plan files", () => {
  assert.deepEqual(buildOrderFileFilter(["order-1"], [VIDEO_MIME]).mimeType, [
    "video/mp4",
  ]);
  assert.deepEqual(buildOrderFileFilter(["order-1"], [PLAN_MIME]).mimeType, [
    "application/json",
  ]);
});

test("order file filter matches on the order tags", () => {
  const filter = buildOrderFileFilter(["order-1", "order-2"], [VIDEO_MIME]);
  assert.deepEqual(filter.tagsFilter?.tags, ["order-1", "order-2"]);
  assert.equal(
    filter.tagsFilter?.type,
    VIAM.dataApi.TagsFilterType.MATCH_BY_OR
  );
});

// Constructing the real message is what makes a bogus field a compile error;
// the old plain-object cast silently dropped startTime/endTime, which are not
// fields on Filter at all.
test("order file filter is a real Filter message", () => {
  assert.ok(
    buildOrderFileFilter(["order-1"], [VIDEO_MIME]) instanceof
      VIAM.dataApi.Filter
  );
});

// planRequestTagDir + savePlanRequestAndResponse own both of these formats.
test("plan step tags read back as their step label", () => {
  assert.equal(unslugStep("step_locking_portafilter"), "Locking portafilter");
  assert.equal(unslugStep("step_brewing"), "Brewing");
});

test("plan filenames yield their capture time", () => {
  assert.equal(planTimeLabel("20260917_094831.007_move.json"), "09:48:31.007");
  assert.equal(planTimeLabel("not-a-plan-file.json"), "");
});

// Lexical order on the stamped filename has to equal plan order, because that
// is what the panel sorts by.
test("plan filenames sort lexically into plan order", () => {
  const names = [
    "20260917_094833.812_move.json",
    "20260917_094831.007_move.json",
    "20260917_100002.100_carry.json",
    "20260917_094825.903_carry.json",
  ];
  assert.deepEqual([...names].sort((a, b) => a.localeCompare(b)), [
    "20260917_094825.903_carry.json",
    "20260917_094831.007_move.json",
    "20260917_094833.812_move.json",
    "20260917_100002.100_carry.json",
  ]);
});

test("startOfToday is midnight local time", () => {
  const d = startOfToday();
  assert.equal(d.getHours(), 0);
  assert.equal(d.getMinutes(), 0);
  assert.equal(d.getSeconds(), 0);
  assert.equal(d.getMilliseconds(), 0);
  assert.equal(d.toDateString(), new Date().toDateString());
});

test("today panel has its own key and title", () => {
  assert.equal(panelKey({ kind: "today" }), "today");
  assert.notEqual(panelKey({ kind: "today" }), panelKey({ kind: "errors" }));
  assert.equal(panelTitle({ kind: "today" }), "Today · all machines");
});
