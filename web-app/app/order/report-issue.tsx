"use client";

import { useEffect, useState } from "react";
import {
  getQueue,
  reportIssue,
  type ReportableOrder,
  type ReportWhich,
  type ViamConnection,
} from "../lib/viamClient";
import { DrinkName } from "./drink-name";
import {
  ISSUE_KINDS,
  OTHER_ISSUE,
  SEVERITIES,
  type Severity,
  severityChipClass,
  severityLabel,
} from "./issues";

// The reportable orders change as the queue moves, so they are re-read while
// the screen is open rather than once.
const POLL_MS = 3000;
const THANKS_DISMISS_MS = 4000;
const MAX_DESCRIPTION = 500;

function minutesAgo(iso: string | undefined): string {
  if (!iso) return "";
  const min = Math.round((Date.now() - new Date(iso).getTime()) / 60_000);
  if (min < 1) return "just now";
  if (min === 1) return "1 min ago";
  if (min < 60) return `${min} min ago`;
  return new Date(iso).toLocaleTimeString(undefined, {
    hour: "2-digit",
    minute: "2-digit",
  });
}

function SeverityChip({ severity }: { severity: string }) {
  return (
    <span
      className={`inline-block px-2 py-0.5 rounded-full border text-xs font-mono uppercase tracking-wider ${severityChipClass(severity)}`}
    >
      {severityLabel(severity)}
    </span>
  );
}

function OrderCard({
  title,
  order,
  status,
  selected,
  onSelect,
}: {
  title: string;
  order: ReportableOrder | undefined;
  status: string;
  selected: boolean;
  onSelect: () => void;
}) {
  return (
    <button
      type="button"
      disabled={!order}
      onClick={onSelect}
      aria-pressed={selected}
      className={`press flex-1 min-w-0 text-left px-5 py-4 rounded-2xl border-2 transition-colors disabled:cursor-not-allowed disabled:opacity-50 ${
        selected
          ? "border-neutral-900 bg-neutral-900 text-white"
          : "border-neutral-200 bg-white text-neutral-900 hover:bg-neutral-50"
      }`}
    >
      <div
        className={`text-xs font-mono uppercase tracking-widest mb-1 ${selected ? "text-neutral-300" : "text-neutral-400"}`}
      >
        {title}
      </div>
      {order ? (
        <>
          <div className="text-lg font-semibold truncate">
            <DrinkName drink={order.drink} beans={order.beans} />
          </div>
          <div
            className={`text-sm truncate ${selected ? "text-neutral-300" : "text-neutral-500"}`}
          >
            {order.customer_name || "Anonymous"} · {status}
          </div>
        </>
      ) : (
        <div className="text-sm text-neutral-500">None</div>
      )}
    </button>
  );
}

export function ReportIssue({
  viamConn,
  connected,
  onBack,
}: {
  viamConn: ViamConnection | null;
  connected: boolean;
  onBack: () => void;
}) {
  const [orders, setOrders] = useState<{
    current?: ReportableOrder;
    previous?: ReportableOrder;
  } | null>(null);
  const [which, setWhich] = useState<ReportWhich | null>(null);
  const [issue, setIssue] = useState<string | null>(null);
  const [description, setDescription] = useState("");
  const [severity, setSeverity] = useState<Severity | null>(null);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [done, setDone] = useState(false);

  useEffect(() => {
    if (!viamConn) return;
    let cancelled = false;
    const load = async () => {
      try {
        const q = await getQueue(viamConn);
        if (cancelled) return;
        const next = q.reportable_orders ?? {};
        setOrders(next);
        // Default to the only order there is; never switch away from one the
        // customer picked while it still exists.
        setWhich((w) => {
          if (w && next[w]) return w;
          if (next.current && !next.previous) return "current";
          if (next.previous && !next.current) return "previous";
          return null;
        });
      } catch (e) {
        console.error("failed to load reportable orders:", e);
      }
    };
    load();
    const id = setInterval(load, POLL_MS);
    return () => {
      cancelled = true;
      clearInterval(id);
    };
  }, [viamConn]);

  useEffect(() => {
    if (!done) return;
    const t = setTimeout(onBack, THANKS_DISMISS_MS);
    return () => clearTimeout(t);
  }, [done, onBack]);

  const isOther = issue === OTHER_ISSUE;
  const canSubmit =
    connected &&
    !submitting &&
    which !== null &&
    issue !== null &&
    (!isOther || (description.trim() !== "" && severity !== null));

  async function submit() {
    if (!viamConn || !canSubmit || !which || !issue) return;
    setSubmitting(true);
    setError(null);
    try {
      await reportIssue(viamConn, {
        which,
        issue,
        ...(isOther && { description: description.trim(), severity: severity! }),
      });
      setDone(true);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setSubmitting(false);
    }
  }

  if (done) {
    return (
      <main className="relative h-full bg-white flex flex-col items-center justify-center p-8 font-sans">
        <p className="anim-pop text-6xl mb-6">🙏</p>
        <h1 className="anim-in text-3xl font-semibold text-neutral-900 text-center mb-2">
          Thanks for letting us know
        </h1>
        <p
          className="anim-in text-neutral-500 text-center"
          style={{ animationDelay: "150ms" }}
        >
          The team will look into it.
        </p>
      </main>
    );
  }

  const noOrders = orders !== null && !orders.current && !orders.previous;

  return (
    <main className="relative h-full bg-white flex flex-col items-center overflow-y-auto p-4 pt-20 pb-10 font-sans">
      <button
        type="button"
        onClick={onBack}
        aria-label="Go back"
        className="anim-in absolute left-6 top-6 h-11 w-11 rounded-full border border-neutral-200 bg-white text-neutral-900 transition-colors hover:bg-neutral-50 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-neutral-400"
      >
        <svg
          aria-hidden="true"
          viewBox="0 0 24 24"
          className="mx-auto h-5 w-5"
          fill="none"
          stroke="currentColor"
          strokeWidth="2"
          strokeLinecap="round"
          strokeLinejoin="round"
        >
          <path d="M15 18l-6-6 6-6" />
        </svg>
      </button>

      <div className="w-full max-w-[640px] flex flex-col gap-8">
        <h1 className="anim-in text-3xl tracking-tight font-semibold text-neutral-900 text-center">
          Report a problem
        </h1>

        <section className="anim-in flex flex-col gap-3" style={{ animationDelay: "80ms" }}>
          <h2 className="text-sm font-mono uppercase tracking-widest text-neutral-500">
            1 · Which order?
          </h2>
          {orders === null ? (
            <p className="text-neutral-500">Loading orders…</p>
          ) : noOrders ? (
            <p className="text-neutral-500">
              There is no recent order to report on.
            </p>
          ) : (
            <div className="flex flex-col sm:flex-row gap-3">
              <OrderCard
                title="Current order"
                order={orders.current}
                status="being made"
                selected={which === "current"}
                onSelect={() => setWhich("current")}
              />
              <OrderCard
                title="Previous order"
                order={orders.previous}
                status={`finished ${minutesAgo(orders.previous?.completed_at)}`}
                selected={which === "previous"}
                onSelect={() => setWhich("previous")}
              />
            </div>
          )}
        </section>

        <section className="anim-in flex flex-col gap-3" style={{ animationDelay: "160ms" }}>
          <h2 className="text-sm font-mono uppercase tracking-widest text-neutral-500">
            2 · What happened?
          </h2>
          <div className="grid grid-cols-1 sm:grid-cols-2 gap-2">
            {ISSUE_KINDS.map((k) => (
              <button
                key={k.key}
                type="button"
                onClick={() => setIssue(k.key)}
                aria-pressed={issue === k.key}
                className={`press flex items-center justify-between gap-3 px-5 py-3 rounded-2xl border-2 text-left transition-colors ${
                  issue === k.key
                    ? "border-neutral-900 bg-neutral-50"
                    : "border-neutral-200 bg-white hover:bg-neutral-50"
                }`}
              >
                <span className="font-medium text-neutral-900">{k.label}</span>
                <SeverityChip severity={k.severity} />
              </button>
            ))}
            <button
              type="button"
              onClick={() => setIssue(OTHER_ISSUE)}
              aria-pressed={isOther}
              className={`press flex items-center justify-between gap-3 px-5 py-3 rounded-2xl border-2 border-dashed text-left transition-colors ${
                isOther
                  ? "border-neutral-900 bg-neutral-50"
                  : "border-neutral-300 bg-white hover:bg-neutral-50"
              }`}
            >
              <span className="font-medium text-neutral-900">
                Something else…
              </span>
            </button>
          </div>
        </section>

        {isOther && (
          <section className="anim-in flex flex-col gap-3">
            <h2 className="text-sm font-mono uppercase tracking-widest text-neutral-500">
              3 · Tell us more
            </h2>
            <textarea
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              maxLength={MAX_DESCRIPTION}
              rows={3}
              autoFocus
              placeholder="What went wrong?"
              className="w-full px-5 py-3 bg-neutral-50 border-2 border-neutral-200 rounded-2xl text-neutral-900 text-base outline-none focus:border-neutral-400 transition-colors font-sans resize-none"
            />
            <div className="flex flex-wrap items-center gap-2">
              <span className="text-sm text-neutral-500 mr-1">How bad was it?</span>
              {SEVERITIES.map((s) => (
                <button
                  key={s}
                  type="button"
                  onClick={() => setSeverity(s)}
                  aria-pressed={severity === s}
                  className={`press px-4 py-2 rounded-full border-2 text-sm font-medium transition-colors ${
                    severity === s
                      ? `${severityChipClass(s)} !border-neutral-900`
                      : "border-neutral-200 bg-white text-neutral-700 hover:bg-neutral-50"
                  }`}
                >
                  {severityLabel(s)}
                </button>
              ))}
            </div>
          </section>
        )}

        {error && (
          <p className="text-red-500 text-sm text-center">{error}</p>
        )}

        <button
          type="button"
          onClick={submit}
          disabled={!canSubmit}
          className="press self-center px-16 py-4 text-lg font-medium bg-neutral-900 text-white rounded-full transition-colors hover:bg-neutral-800 disabled:bg-neutral-300 disabled:cursor-not-allowed"
        >
          {submitting ? "Sending…" : "Send report"}
        </button>
      </div>
    </main>
  );
}
