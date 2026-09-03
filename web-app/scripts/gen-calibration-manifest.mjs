// Generates app/lib/calibrationManifest.ts from live machine configs in the
// Viam app. Run it after a pose is added, removed, renamed, or re-baselined:
//
//   cd web-app && npm run gen:manifest -- <partId> [<partId> ...]
//   make web-app-manifest                                   (from the repo root)
//
// Auth reuses the `viam` CLI's token, so `viam login` is the only setup.
//
// The manifest records structure only — which poses hang off which frame, and
// which of those follow another pose. No coordinates: the numbers an operator
// wants are the live frame positions the page reads off the running machine,
// not anything that could be baked in here.
//
// Where the poses come from:
//
//   * The fragment is the source of truth. A pose switch defined there is read
//     from there, variable placeholders and all — a `pose_value` holding a
//     `$variable` still says "this pose is set by hand", which is all the
//     manifest needs, so variables are never resolved.
//   * `fragment_mods` are ignored on purpose. A machine-level override of a
//     switch's pose array is treated as temporary scaffolding, not as the
//     shape the fleet is meant to have.
//   * The machine's own config is consulted only for switches the fragment
//     does not define at all — that is how the two observe switches exist.
//
// Variables ARE resolved in one place: a switcher *name*, so a fragment that
// parameterizes which switch the coffee service drives still works.

import { readFileSync, writeFileSync } from "node:fs";
import { homedir } from "node:os";
import { createViamClient } from "@viamrobotics/sdk";

const OUT = new URL("../app/lib/calibrationManifest.ts", import.meta.url);
const SWITCH_MODEL = "multi-poses-execution-switch";
const COFFEE_MODEL = "viam:beanjamin:coffee";

// The coffee service's switcher attributes, in brew order. The manifest keeps
// this order so the page can render frames as they come rather than re-sorting.
const SWITCHER_ATTRS = [
  "pose_switcher_name",
  "claws_pose_switcher_name",
  "camera_observe_pose_switcher_name",
  "glass_observe_pose_switcher_name",
];

/**
 * Resolves a fragment value that may be a `{"$variable": {"name": …}}`
 * placeholder against the machine's variable values. Used only where a plain
 * string is required (switcher names); pose values are left as-is.
 */
function literal(value, variables, what) {
  const ref = value?.$variable?.name;
  if (ref !== undefined) {
    if (!(ref in variables)) {
      throw new Error(`${what}: fragment variable ${ref} has no value`);
    }
    return variables[ref];
  }
  return value;
}

/** Indexes components by name, first definition winning. */
function byName(entries) {
  const index = new Map();
  for (const entry of entries) {
    if (entry?.name !== undefined && !index.has(entry.name)) {
      index.set(entry.name, entry);
    }
  }
  return index;
}

async function buildMachine(client, partId) {
  const { part } = await client.appClient.getRobotPart(partId);
  if (!part) throw new Error(`no part ${partId}`);
  const raw = part.robotConfig.toJson();

  // Fragment definitions first, so byName() resolves a name to the fragment's
  // copy and the machine's own config only supplies what the fragment omits.
  // A part can list the same fragment more than once; fetching it repeatedly
  // changes nothing but costs a round-trip each.
  const components = [];
  const services = [];
  const variables = {};
  const fetched = new Set();
  for (const ref of raw.fragments ?? []) {
    // A part can list one fragment several times — each listing is a separate
    // instantiation with its own variable values, so variables are collected
    // per listing even though the definition is only worth fetching once.
    // Collisions between listings resolve last-wins; the only variables this
    // script reads are switcher names, which come from a singly-listed
    // fragment, so the ambiguity never reaches the manifest.
    Object.assign(variables, ref.variables ?? {});
    if (fetched.has(ref.id)) continue;
    fetched.add(ref.id);

    const fragment = await client.appClient.getFragment(ref.id);
    if (!fragment) throw new Error(`${partId}: no fragment ${ref.id}`);
    const fc = fragment.fragment.toJson();
    components.push(...(fc.components ?? []));
    services.push(...(fc.services ?? []));
  }
  components.push(...(raw.components ?? []));
  services.push(...(raw.services ?? []));

  const componentsByName = byName(components);
  const coffee = services.find((s) =>
    String(s.model ?? "").includes(COFFEE_MODEL),
  );
  if (!coffee) throw new Error(`${partId}: no ${COFFEE_MODEL} service`);

  const frames = [];
  for (const attr of SWITCHER_ATTRS) {
    const configured = coffee.attributes?.[attr];
    if (configured === undefined) continue;

    const name = literal(configured, variables, `${partId}: ${attr}`);
    if (typeof name !== "string") {
      throw new Error(
        `${partId}: ${attr} is not a switch name: ${JSON.stringify(name)}`,
      );
    }

    const component = componentsByName.get(name);
    if (!component) {
      throw new Error(`${partId}: ${attr} names a missing component ${name}`);
    }
    if (!String(component.model ?? "").includes(SWITCH_MODEL)) {
      throw new Error(`${partId}: ${name} is not a ${SWITCH_MODEL}`);
    }

    const frame = literal(
      component.attributes?.component_name,
      variables,
      `${partId}: ${name}.component_name`,
    );
    if (typeof frame !== "string") {
      throw new Error(`${partId}: ${name} has no component_name to move`);
    }

    const poses = component.attributes?.poses ?? [];
    if (poses.length === 0) {
      throw new Error(`${partId}: ${name} carries no poses`);
    }

    frames.push({
      frame,
      switch: name,
      // A pose_value that is a variable placeholder still counts: it is set by
      // hand, which is the only distinction the manifest draws.
      configure: poses
        .filter((p) => p.pose_value !== undefined)
        .map((p) => p.pose_name)
        .sort(),
      derived: poses
        .filter((p) => p.pose_value === undefined)
        .map((p) => ({ pose: p.pose_name, baseline: p.baseline }))
        .sort((a, b) => a.pose.localeCompare(b.pose)),
    });
  }

  if (frames.length === 0) {
    throw new Error(`${partId}: ${COFFEE_MODEL} names no pose switches`);
  }

  return {
    machine: part.name,
    partId,
    generatedAt: new Date().toISOString(),
    frames,
  };
}

const partIds = process.argv.slice(2);
if (partIds.length === 0) {
  console.error("usage: npm run gen:manifest -- <partId> [<partId> ...]");
  process.exit(1);
}

const cliConfig = JSON.parse(
  readFileSync(`${homedir()}/.viam/cached_cli_config.json`, "utf8"),
);
const client = await createViamClient({
  credentials: { type: "access-token", payload: cliConfig.auth.access_token },
});

const machines = [];
for (const partId of partIds) {
  machines.push(await buildMachine(client, partId));
  console.error(`resolved ${partId}`);
}

writeFileSync(
  OUT,
  `// GENERATED by scripts/gen-calibration-manifest.mjs — do not edit by hand.
//
// Regenerate after adding, removing, renaming, or re-baselining a pose:
//   make web-app-manifest
//
// Structure only. Pose values are deliberately absent — the page reads live
// frame positions off the running machine instead.

import type { MachineManifest } from "./calibration";

export const MANIFEST: MachineManifest[] = ${JSON.stringify(machines, null, 2)};
`,
);
console.error(`wrote ${OUT.pathname}`);
process.exit(0);
