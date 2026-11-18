"use client";

import {
  type Incident,
  type MachineReliability,
  RELIABILITY_WINDOW_DAYS,
} from "./reliability";

const BAR_AREA_HEIGHT = 160;
const MIN_BAR_HEIGHT = 3;

function formatMdbf(r: MachineReliability): string {
  if (r.mdbf === null) return `≥ ${r.drinks}`;
  return r.mdbf >= 10 ? Math.round(r.mdbf).toString() : r.mdbf.toFixed(1);
}

function formatStreak(r: MachineReliability): string {
  return `${r.sinceLastIsLowerBound ? "≥ " : ""}${r.drinksSinceLastIncident}`;
}

function shortDate(d: Date): string {
  return d.toLocaleDateString(undefined, { month: "short", day: "numeric" });
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

function Bar({
  value,
  label,
  max,
  title,
  streak,
}: {
  value: number;
  label: string;
  max: number;
  title: string;
  streak?: boolean;
}) {
  const height = Math.max(
    MIN_BAR_HEIGHT,
    Math.round((value / max) * BAR_AREA_HEIGHT)
  );
  return (
    <div
      title={title}
      className="flex-1 min-w-0 max-w-12 flex flex-col items-center justify-end h-full"
    >
      <span
        className={`text-sm font-semibold tabular-nums leading-none mb-1 ${
          streak ? "text-green-700" : "text-neutral-800"
        }`}
      >
        {label}
      </span>
      <div
        className={
          streak
            ? "w-full rounded-t-sm border-2 border-b-0 border-green-600 bg-green-100"
            : "w-full rounded-t-sm bg-neutral-400 hover:bg-neutral-600"
        }
        style={{ height }}
      />
    </div>
  );
}

// One bar per incident, its height the drinks served since the previous one,
// then a green bar for the streak still running. Taller bars over time is the
// trend the metric is meant to show.
function IncidentBars({ r }: { r: MachineReliability }) {
  const max = Math.max(
    1,
    r.drinksSinceLastIncident,
    ...r.incidents.map((i) => i.drinksBefore)
  );
  const first = r.incidents[0];
  return (
    <div>
      {/* Room above the tallest bar for its label. */}
      <div
        className="flex items-end gap-1 border-b border-neutral-300"
        style={{ height: BAR_AREA_HEIGHT + 24 }}
      >
        {r.incidents.map((i) => (
          <Bar
            key={i.start.getTime()}
            value={i.drinksBefore}
            label={`${i.drinksBeforeIsLowerBound ? "≥" : ""}${i.drinksBefore}`}
            max={max}
            title={incidentTitle(i)}
          />
        ))}
        <Bar
          value={r.drinksSinceLastIncident}
          label={formatStreak(r)}
          max={max}
          title={`Current streak · ${formatStreak(r)} drinks`}
          streak
        />
      </div>
      <div className="flex justify-between text-xs text-neutral-500 mt-1">
        <span>{first ? shortDate(first.start) : ""}</span>
        <span>now</span>
      </div>
    </div>
  );
}

function Stat({
  value,
  label,
  accent,
}: {
  value: string;
  label: string;
  accent?: boolean;
}) {
  return (
    <div>
      <div
        className={`text-4xl font-semibold tabular-nums ${
          accent ? "text-green-700" : ""
        }`}
      >
        {value}
      </div>
      <div className="text-sm text-neutral-500">{label}</div>
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
    <li className="rounded-md border border-neutral-200 p-4">
      <div className="flex flex-wrap items-end gap-x-10 gap-y-3 mb-4">
        <div className="font-semibold text-lg self-start w-full">{name}</div>
        <Stat value={formatMdbf(r)} label="drinks between failures" />
        <Stat
          value={formatStreak(r)}
          label="drinks since last incident"
          accent
        />
        <div className="text-sm text-neutral-500 tabular-nums">
          {r.drinks} drinks · {r.incidents.length}{" "}
          {r.incidents.length === 1 ? "incident" : "incidents"}
          {r.failures > r.incidents.length &&
            ` (${r.failures} failed attempts)`}
          {r.topSteps.length > 0 && (
            <div>
              Fails at:{" "}
              {r.topSteps
                .slice(0, 3)
                .map((s) => `${s.step} (${s.count})`)
                .join(", ")}
            </div>
          )}
        </div>
      </div>
      <IncidentBars r={r} />
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
    <section className="mt-6">
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
        <ul className="m-0 p-0 list-none space-y-4">
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
