"use client";

import { useEffect, useState } from "react";
import Link from "next/link";
import { useSearchParams } from "next/navigation";
import {
  contributePoints,
  getCurrentUserEmail,
  getRewards,
  type Reward,
  type RewardsState,
} from "./lib/viamClient";
import { useViamConnection } from "./lib/useViamConnection";

// Others contribute too, so progress is re-read while the page is open.
const REFRESH_MS = 15_000;

// Open rewards come first, nearest to unlocking (by fraction funded) at the
// top, ties going to the cheaper reward. Unlocked ones sink to the bottom since
// they no longer take contributions.
function compareRewards(a: Reward, b: Reward): number {
  if (a.funded !== b.funded) return a.funded ? 1 : -1;
  const progress = (r: Reward) => r.contributed / r.points_required;
  return progress(b) - progress(a) || a.points_required - b.points_required;
}

function errorText(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

function ProgressBar({ reward }: { reward: Reward }) {
  const pct = Math.min(
    100,
    (reward.contributed / reward.points_required) * 100,
  );
  return (
    <div className="h-2 rounded-full bg-neutral-100 overflow-hidden">
      <div
        className={`h-full rounded-full ${reward.funded ? "bg-green-500" : "bg-neutral-900"}`}
        style={{ width: `${pct}%` }}
      />
    </div>
  );
}

function ContributeForm({
  reward,
  balance,
  onContribute,
}: {
  reward: Reward;
  balance: number;
  onContribute: (points: number) => Promise<void>;
}) {
  const max = Math.min(balance, reward.remaining);
  const [requested, setPoints] = useState(1);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // Clamped on read: the balance and the reward's remaining points both change
  // underneath the field as the page refreshes.
  const points = Math.max(1, Math.min(requested, max));

  if (max < 1) {
    return (
      <p className="text-xs text-neutral-400">
        No points to contribute yet — every drink you order earns one.
      </p>
    );
  }

  const submit = async () => {
    setBusy(true);
    setError(null);
    try {
      await onContribute(points);
    } catch (e) {
      setError(errorText(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="flex flex-wrap items-center gap-2">
      <input
        type="number"
        min={1}
        max={max}
        step={1}
        value={points}
        onChange={(e) => setPoints(Math.floor(Number(e.target.value)) || 1)}
        className="w-20 px-2 py-1 text-sm border border-neutral-300 rounded"
        aria-label={`Points to contribute to ${reward.name}`}
      />
      <button
        onClick={() => setPoints(max)}
        className="px-2 py-1 text-xs rounded border border-neutral-200 text-neutral-600 hover:bg-neutral-100"
      >
        Max ({max})
      </button>
      <button
        onClick={submit}
        disabled={busy || points < 1 || points > max}
        className="px-3 py-1 text-sm rounded bg-neutral-900 text-white hover:bg-neutral-700 disabled:opacity-40"
      >
        {busy
          ? "Contributing…"
          : `Contribute ${points} point${points === 1 ? "" : "s"}`}
      </button>
      {error && <span className="text-xs text-red-600">{error}</span>}
    </div>
  );
}

function RewardCard({
  reward,
  balance,
  me,
  onContribute,
}: {
  reward: Reward;
  balance: number;
  me: string;
  onContribute: (points: number) => Promise<void>;
}) {
  const [showContributors, setShowContributors] = useState(false);
  const mine = reward.contributors.find((c) => c.email === me)?.points ?? 0;

  return (
    <section
      className={`border rounded-lg p-4 ${mine > 0 ? "border-sky-200 bg-sky-50" : "border-neutral-200 bg-white"}`}
    >
      <header className="flex flex-wrap items-baseline gap-x-3 gap-y-1 mb-1">
        <h2 className="text-base font-semibold text-neutral-900">
          {reward.name}
        </h2>
        {reward.funded ? (
          <span className="px-2 py-0.5 text-xs rounded-full bg-green-100 text-green-800">
            Unlocked
            {reward.funded_at &&
              ` · ${new Date(reward.funded_at).toLocaleDateString()}`}
          </span>
        ) : (
          <span className="text-xs text-neutral-500">
            {reward.remaining} point{reward.remaining === 1 ? "" : "s"} to go
          </span>
        )}
        <span className="ml-auto text-sm tabular-nums text-neutral-700">
          {reward.contributed} / {reward.points_required}
        </span>
      </header>
      {reward.description && (
        <p className="text-sm text-neutral-500 mb-2">{reward.description}</p>
      )}
      <div className="mb-3">
        <ProgressBar reward={reward} />
      </div>

      {!reward.funded && (
        <div className="mb-2">
          <ContributeForm
            reward={reward}
            balance={balance}
            onContribute={onContribute}
          />
        </div>
      )}

      {reward.contributors.length > 0 && (
        <div className="text-xs text-neutral-500">
          <button
            onClick={() => setShowContributors((v) => !v)}
            className="hover:text-neutral-900"
          >
            {showContributors ? "▾" : "▸"} {reward.contributors.length}{" "}
            contributor{reward.contributors.length === 1 ? "" : "s"}
            {mine > 0 && ` · you gave ${mine}`}
          </button>
          {showContributors && (
            <ul className="mt-1 ml-3 space-y-0.5">
              {reward.contributors.map((c) => (
                <li key={c.email} className="flex gap-2">
                  <span
                    className={
                      c.email === me ? "font-medium text-neutral-900" : ""
                    }
                  >
                    {c.email}
                  </span>
                  <span className="ml-auto tabular-nums">{c.points}</span>
                </li>
              ))}
            </ul>
          )}
        </div>
      )}
    </section>
  );
}

export function Rewards() {
  const partId = useSearchParams().get("partId") ?? "";
  const { conn, error: connError } = useViamConnection(partId);
  const [email, setEmail] = useState<string | null>(null);
  const [state, setState] = useState<RewardsState | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!conn) return;
    let cancelled = false;
    getCurrentUserEmail(conn)
      .then((e) => !cancelled && setEmail(e))
      .catch(
        (e) =>
          !cancelled && setError(`Couldn't tell who you are: ${errorText(e)}`),
      );
    return () => {
      cancelled = true;
    };
  }, [conn]);

  useEffect(() => {
    if (!conn || !email) return;
    let cancelled = false;
    const read = async () => {
      try {
        const next = await getRewards(conn, email);
        if (cancelled) return;
        setState(next);
        setError(null);
      } catch (e) {
        if (!cancelled) setError(errorText(e));
      }
    };
    read();
    const timer = setInterval(() => {
      if (!document.hidden) read();
    }, REFRESH_MS);
    return () => {
      cancelled = true;
      clearInterval(timer);
    };
  }, [conn, email]);

  const contribute = async (rewardId: string, points: number) => {
    if (!conn || !email) return;
    const res = await contributePoints(conn, email, rewardId, points);
    setState(
      (s) =>
        s && {
          account: res.account,
          rewards: s.rewards.map((r) => (r.id === rewardId ? res.reward : r)),
        },
    );
  };

  const shownError = connError ?? error;

  return (
    <main className="max-w-3xl mx-auto p-6">
      <Link
        href="/"
        className="inline-block mb-3 text-sm text-neutral-500 hover:text-neutral-900 transition-colors"
      >
        ← Back to Fleet Dashboard
      </Link>
      <h1 className="text-xl font-semibold text-neutral-900 mb-1">Rewards</h1>
      <p className="text-sm text-neutral-500 mb-5">
        Every drink you order with your email earns a point. Pool them with
        everyone else&apos;s to unlock upgrades to the barista setup.
      </p>

      {!partId && (
        <p className="text-sm text-red-600">
          No machine selected — open this page from the fleet dashboard.
        </p>
      )}
      {shownError && (
        <div className="mb-4 px-3 py-2 rounded-lg border border-red-200 bg-red-50 text-sm text-red-700">
          {shownError}
        </div>
      )}

      {state && (
        <>
          <section className="mb-6 flex flex-wrap items-baseline gap-x-4 gap-y-1 px-4 py-3 rounded-lg bg-neutral-900 text-white">
            <span className="text-3xl font-semibold tabular-nums">
              {state.account.points}
            </span>
            <span className="text-sm">
              point{state.account.points === 1 ? "" : "s"} to spend
            </span>
            <span className="ml-auto text-xs text-neutral-400">
              {state.account.email} · {state.account.earned} earned ·{" "}
              {state.account.contributed} contributed
            </span>
          </section>

          {state.rewards.length === 0 ? (
            <p className="text-sm text-neutral-500">
              No rewards are set up yet.
            </p>
          ) : (
            <div className="space-y-4">
              {[...state.rewards].sort(compareRewards).map((r) => (
                <RewardCard
                  key={r.id}
                  reward={r}
                  balance={state.account.points}
                  me={state.account.email}
                  onContribute={(points) => contribute(r.id, points)}
                />
              ))}
            </div>
          )}
        </>
      )}
      {!state && !shownError && partId && (
        <p className="text-sm text-neutral-400">Loading…</p>
      )}
    </main>
  );
}
