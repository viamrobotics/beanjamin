"use client";

import {
  type Incident,
  type MachineReliability,
  RELIABILITY_WINDOW_DAYS,
} from "./reliability";

const BAR_AREA_HEIGHT = 56;
const MIN_BAR_HEIGHT = 2;

function formatMdbf(r: MachineReliability): string {
  if (r.mdbf === null) return `≥ ${r.drinks}`;
  return r.mdbf >= 10 ? Math.round(r.mdbf).toString() : r.mdbf.toFixed(1);
}

function incidentTitle(i: Incident): string {
  const when = i.start.toLocaleString(undefined, {
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
  const attempts = i.failures === 1 ? "1 attempt" : `${i.failures} attempts`;
  const before = `${i.drinksBeforeIsLowerBound ? "≥ " : ""}${i.drinksBefore} drinks before`;
  return `${when} · ${i.failedStep} · ${attempts} · ${before}`;
}

// One bar per incident, its height the drinks served since the previous one,
// then a hollow bar for the streak still running. Taller bars over time is the
// trend the metric is meant to show.
function IncidentBars({ r }: { r: MachineReliability }) {
  const streakTitle = `Current streak · ${r.sinceLastIsLowerBound ? "≥ " : ""}${r.drinksSinceLastIncident} drinks`;
  const max = Math.max(
    1,
    r.drinksSinceLastIncident,
    ...r.incidents.map((i) => i.drinksBefore)
  );
  const height = (n: number) =>
    Math.max(MIN_BAR_HEIGHT, Math.round((n / max) * BAR_AREA_HEIGHT));
  return (
    <div
      className="flex items-end gap-0.5 border-b border-neutral-200"
      style={{ height: BAR_AREA_HEIGHT }}
    >
      {r.incidents.map((i) => (
        <div
          key={i.start.getTime()}
          title={incidentTitle(i)}
          className="flex-1 max-w-3 rounded-t-sm bg-neutral-400 hover:bg-neutral-600"
          style={{ height: height(i.drinksBefore) }}
        />
      ))}
      <div
        title={streakTitle}
        className="flex-1 max-w-3 rounded-t-sm border border-b-0 border-green-600 bg-green-50"
        style={{ height: height(r.drinksSinceLastIncident) }}
      />
    </div>
  );
}

function ReliabilityCard({
  name,
  r,
}: {
  name: string;
  r: MachineReliability;
}) {
  return (
    <li className="w-72 rounded-md border border-neutral-200 p-4">
      <div className="font-semibold">{name}</div>
      <div className="mt-1 flex items-baseline gap-2">
        <span className="text-3xl font-semibold tabular-nums">
          {formatMdbf(r)}
        </span>
        <span className="text-sm text-neutral-500">drinks between failures</span>
      </div>
      <p className="text-sm text-neutral-500 mt-1 mb-3 tabular-nums">
        {r.drinks} drinks · {r.incidents.length}{" "}
        {r.incidents.length === 1 ? "incident" : "incidents"}
        {r.failures > r.incidents.length && ` (${r.failures} failed attempts)`}
      </p>
      <IncidentBars r={r} />
      <p className="text-sm mt-2 tabular-nums">
        Since last incident:{" "}
        <strong>
          {r.sinceLastIsLowerBound ? "≥ " : ""}
          {r.drinksSinceLastIncident}
        </strong>{" "}
        drinks
      </p>
      {r.topSteps.length > 0 && (
        <p className="text-sm text-neutral-500 mt-1">
          Fails at:{" "}
          {r.topSteps
            .slice(0, 3)
            .map((s) => `${s.step} (${s.count})`)
            .join(", ")}
        </p>
      )}
    </li>
  );
}

export function ReliabilityPanel({
  reliability,
  machineNameById,
}: {
  reliability: MachineReliability[] | null;
  machineNameById: Map<string, string>;
}) {
  return (
    <section className="mb-6">
      <h2 className="text-xl font-semibold text-neutral-900">Reliability</h2>
      <p className="text-sm text-neutral-500 mb-3">
        Mean drinks between failures over the last {RELIABILITY_WINDOW_DAYS}{" "}
        days. An incident is a run of failed or cancelled orders with no
        successful drink between them; each bar is the drinks served before
        one, the green bar the current streak.
      </p>
      {reliability === null ? (
        <p className="text-neutral-500">Loading reliability…</p>
      ) : reliability.length === 0 ? (
        <p className="text-neutral-500">No orders yet.</p>
      ) : (
        <ul className="m-0 p-0 list-none flex flex-wrap gap-4">
          {reliability.map((r) => (
            <ReliabilityCard
              key={r.robotId}
              name={machineNameById.get(r.robotId) ?? r.robotId}
              r={r}
            />
          ))}
        </ul>
      )}
    </section>
  );
}
