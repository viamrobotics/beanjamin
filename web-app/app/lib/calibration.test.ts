import { test } from "node:test";
import assert from "node:assert/strict";
import { formatCapture, manifestAge } from "./calibration";
import { MANIFEST } from "./calibrationManifest";

// Asserted against the checked-in manifest, generated from a real machine.
const manifest = MANIFEST[0];
const byName = new Map(manifest.frames.map((f) => [f.switch, f]));
const frame = (switchName: string) => byName.get(switchName)!;

test("frames are listed per switch, in brew order", () => {
  assert.deepEqual(
    manifest.frames.map((f) => [f.frame, f.switch]),
    [
      ["filter", "filter-poses-switch"],
      ["grip-point", "claws-position-switch"],
      ["cam", "camera-observation-switch"],
      ["cam", "glass-observe-switch"],
    ],
  );
});

test("poses you set are separated from poses that follow a baseline", () => {
  const filter = frame("filter-poses-switch");
  assert.deepEqual(filter.configure, [
    "cleaning_brush_active",
    "cleaning_scrapper_active",
    "coffee_in",
    "decaf_grinder_activate",
    "grinder_activate",
    "home",
    "purge_approach",
    "tamper_activate",
  ]);
  // grinder_approach is 15mm off grinder_activate; setting it directly would
  // cut it loose from the pose it exists to track.
  assert.deepEqual(
    filter.derived.find((d) => d.pose === "grinder_approach"),
    { pose: "grinder_approach", baseline: "grinder_activate" },
  );
});

test("no pose appears in both lists", () => {
  for (const f of manifest.frames) {
    const derived = new Set(f.derived.map((d) => d.pose));
    assert.ok(f.configure.every((p) => !derived.has(p)));
  }
});

test("every baseline names a pose on the same switch", () => {
  for (const f of manifest.frames) {
    const known = new Set([...f.configure, ...f.derived.map((d) => d.pose)]);
    for (const d of f.derived) {
      assert.ok(known.has(d.baseline), `${f.switch}: ${d.baseline} not found`);
    }
  }
});

test("poses come from the fragment, not from machine-level overrides", () => {
  // This machine once overrode components.filter-poses-switch.attributes.poses
  // wholesale, which dropped the purge poses the fragment declares. Overrides
  // are treated as temporary scaffolding and ignored, so the fragment's poses
  // are what the manifest reports either way.
  const filter = frame("filter-poses-switch");
  const all = [...filter.configure, ...filter.derived.map((d) => d.pose)];
  assert.ok(all.includes("purge_approach"));
  assert.ok(all.includes("purge_press"));
});

test("switches the fragment does not define fall back to the machine", () => {
  // The observe switches are configured on the machine itself, so ignoring
  // machine config wholesale would drop them.
  assert.equal(frame("camera-observation-switch").frame, "cam");
  assert.ok(frame("camera-observation-switch").configure.length > 0);
});

test("both observe switches move the cam frame", () => {
  assert.equal(frame("camera-observation-switch").frame, "cam");
  assert.equal(frame("glass-observe-switch").frame, "cam");
  // Every observation vantage is set by hand; none derive from another.
  assert.deepEqual(frame("camera-observation-switch").derived, []);
});

test("age escalates but never reports the list as trustworthy", () => {
  // No "fresh" level: a same-day capture is still wrong if a pose changed
  // after it was generated, so the page must warn regardless of age.
  const at = "2026-01-01T00:00:00.000Z";
  const day = (n: number) => Date.parse(at) + n * 86_400_000;
  assert.deepEqual(manifestAge(at, day(0)), { days: 0, level: "warn" });
  assert.deepEqual(manifestAge(at, day(29)), { days: 29, level: "warn" });
  assert.deepEqual(manifestAge(at, day(30)), { days: 30, level: "stale" });
});

test("an unreadable timestamp reads as stale", () => {
  assert.equal(manifestAge("not a date").level, "stale");
});

test("capture text carries both the time and how long ago it was", () => {
  const at = "2026-01-01T00:00:00.000Z";
  const day = (n: number) => Date.parse(at) + n * 86_400_000;
  assert.match(formatCapture(at, manifestAge(at, day(0))), / · today$/);
  assert.match(formatCapture(at, manifestAge(at, day(1))), / · 1 day ago$/);
  assert.match(formatCapture(at, manifestAge(at, day(5))), / · 5 days ago$/);
  assert.equal(
    formatCapture("nope", manifestAge("nope")),
    "at an unreadable date",
  );
});

test("the checked-in manifest carries a parseable timestamp", () => {
  assert.ok(!Number.isNaN(Date.parse(manifest.generatedAt)));
});
