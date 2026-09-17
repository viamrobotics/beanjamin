import { test } from "node:test";
import assert from "node:assert/strict";
import * as VIAM from "@viamrobotics/sdk";
import { buildVideoFilter, startOfToday, panelKey, panelTitle } from "./data";

// The order ID is not a video-only tag — coffee/motion.go tags every saved
// motion-plan JSON with it as well, one per planned motion. Without the
// mime-type filter those plan files come back as clips, which is how this
// broke before: the query still "worked", it just returned the wrong things.
test("video filter restricts to mp4 so plan JSON is excluded", () => {
  const filter = buildVideoFilter(["order-1"]);
  assert.deepEqual(filter.mimeType, ["video/mp4"]);
});

test("video filter matches on the order tags", () => {
  const filter = buildVideoFilter(["order-1", "order-2"]);
  assert.deepEqual(filter.tagsFilter?.tags, ["order-1", "order-2"]);
  assert.equal(
    filter.tagsFilter?.type,
    VIAM.dataApi.TagsFilterType.MATCH_BY_OR
  );
});

// Constructing the real message is what makes a bogus field a compile error;
// the old plain-object cast silently dropped startTime/endTime, which are not
// fields on Filter at all.
test("video filter is a real Filter message", () => {
  assert.ok(buildVideoFilter(["order-1"]) instanceof VIAM.dataApi.Filter);
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
