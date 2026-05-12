"use client";

import { type LeaderboardEntry } from "./data";
import { drinkLabel } from "../order/drinks";

const RANK_EMOJI = ["🥇", "🥈", "🥉", "☕", "☕"];

function LeaderboardList({
  entries,
  emptyMsg,
  formatName,
}: {
  entries: LeaderboardEntry[] | null;
  emptyMsg: string;
  formatName?: (name: string) => string;
}) {
  if (entries === null) return <p className="text-neutral-500">Loading…</p>;
  if (entries.length === 0)
    return <p className="text-neutral-500">{emptyMsg}</p>;
  return (
    <ul className="m-0 p-0 list-none">
      {entries.slice(0, 5).map((e, i) => (
        <li key={e.name} className="py-0.5">
          <span className="mr-2">{RANK_EMOJI[i]}</span>
          {formatName ? formatName(e.name) : e.name} —{" "}
          <strong>{e.count}</strong>
        </li>
      ))}
    </ul>
  );
}

export function Leaderboard({
  customers,
  drinks,
}: {
  customers: LeaderboardEntry[] | null;
  drinks: LeaderboardEntry[] | null;
}) {
  return (
    <section className="mb-6">
      <h2 className="text-xl font-semibold text-neutral-900 mb-3">
        🏆 Leaderboard · last 7 days
      </h2>
      <div className="grid grid-cols-2 gap-6 max-w-xl">
        <div>
          <div className="text-sm text-neutral-500 mb-2">Top customers</div>
          <LeaderboardList entries={customers} emptyMsg="No orders yet." />
        </div>
        <div>
          <div className="text-sm text-neutral-500 mb-2">Top drinks</div>
          <LeaderboardList
            entries={drinks}
            emptyMsg="No drinks yet."
            formatName={drinkLabel}
          />
        </div>
      </div>
    </section>
  );
}
