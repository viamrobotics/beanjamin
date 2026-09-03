// Types for the pose calibration view and its generated manifest
// (calibrationManifest.ts, written by scripts/gen-calibration-manifest.mjs).
//
// The manifest is structure only: which poses hang off which frame, and which
// of those follow another pose. Coordinates are deliberately absent — the
// numbers an operator wants come from the live frame readings the page polls
// off the running machine.

export interface PoseValue {
  x: number;
  y: number;
  z: number;
  o_x: number;
  o_y: number;
  o_z: number;
  theta: number;
}

/** A pose whose value falls out of another one, so it is not set by hand. */
export interface DerivedPose {
  pose: string;
  baseline: string;
}

export interface FrameGroup {
  /** The frame this switch moves — its component_name. */
  frame: string;
  /** The multi-poses-execution-switch carrying these poses. */
  switch: string;
  /** Poses authored with a value of their own: the ones you set. */
  configure: string[];
  derived: DerivedPose[];
}

export interface MachineManifest {
  machine: string;
  partId: string;
  /** ISO timestamp of the generator run that produced this entry. */
  generatedAt: string;
  /** In brew order — filter, claws, then the observation switches. */
  frames: FrameGroup[];
}

const DAY_MS = 86_400_000;
const STALE_DAYS = 30;

export interface ManifestAge {
  days: number;
  /** Always at least "warn": see manifestAge. */
  level: "warn" | "stale";
}

/**
 * How long ago the manifest was captured.
 *
 * There is no "fresh" level on purpose. Nothing in the page can detect that a
 * pose was added or renamed, and a manifest generated this morning is already
 * wrong if someone edited a pose this afternoon — so a same-day capture is not
 * evidence of currency and must not be presented as reassurance. Age only
 * decides how loud the warning is.
 */
export function manifestAge(
  generatedAt: string,
  now = Date.now(),
): ManifestAge {
  const then = Date.parse(generatedAt);
  if (Number.isNaN(then)) return { days: Infinity, level: "stale" };

  const days = Math.max(0, Math.floor((now - then) / DAY_MS));
  return { days, level: days >= STALE_DAYS ? "stale" : "warn" };
}

/** The capture time in the reader's own timezone, plus how long ago that was. */
export function formatCapture(generatedAt: string, age: ManifestAge): string {
  if (!Number.isFinite(age.days)) return "at an unreadable date";

  const when = new Date(generatedAt).toLocaleString(undefined, {
    dateStyle: "medium",
    timeStyle: "short",
  });
  const ago =
    age.days === 0
      ? "today"
      : `${age.days} day${age.days === 1 ? "" : "s"} ago`;
  return `${when} · ${ago}`;
}
