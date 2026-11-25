"use client";

import { useEffect, useState } from "react";
import {
  RECENT_ORDERS_DAYS,
  getQueue,
  getRecentOrders,
  reportIssue,
  type QueueStatus,
  type RecentOrder,
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

// The queue moves every few seconds; finished orders only arrive as the data
// manager syncs, so the tabular read can be much slower.
const QUEUE_POLL_MS = 3000;
const HISTORY_POLL_MS = 30_000;
const THANKS_DISMISS_MS = 4000;
const MAX_DESCRIPTION = 500;

function timeAgo(d: Date): string {
  const min = Math.round((Date.now() - d.getTime()) / 60_000);
  if (min < 1) return "just now";
  if (min < 60) return `${min} min ago`;
  const time = d.toLocaleTimeString(undefined, {
    hour: "2-digit",
    minute: "2-digit",
  });
  if (d.toDateString() === new Date().toDateString()) return time;
  const day = d.toLocaleDateString(undefined, {
    weekday: "short",
    month: "short",
    day: "numeric",
  });
  return `${day}, ${time}`;
}

/**
 * The queue's orders a customer could report on before their reading has
 * synced: the one being made, and those that just finished.
 */
function queueOrders(q: QueueStatus): RecentOrder[] {
  return q.orders
    .filter((o) => o.completed_at !== "" || o.cancellable === false)
    .map((o) => ({
      id: o.id,
      drink: o.drink,
      beans: o.beans ?? "regular",
      customerName: o.customer_name,
      at: new Date(o.completed_at || o.enqueued_at),
      // A just-finished order's outcome is only known once it syncs.
      status: o.completed_at ? ("finished" as const) : ("making" as const),
    }));
}

const STATUS_LABEL: Record<RecentOrder["status"], string> = {
  making: "Being made",
  finished: "Just finished",
  done: "Served",
  failed: "Failed",
};

function SeverityChip({ severity }: { severity: string }) {
  return (
    <span
      className={`inline-block px-2 py-0.5 rounded-full border text-xs font-mono uppercase tracking-wider ${severityChipClass(severity)}`}
    >
      {severityLabel(severity)}
    </span>
  );
}

// Rows span their scroll container's full width, so they press down but never
// scale up on hover the way .press does: that would overflow it sideways.
function OrderRow({
  order,
  selected,
  onSelect,
}: {
  order: RecentOrder;
  selected: boolean;
  onSelect: () => void;
}) {
  return (
    <button
      type="button"
      onClick={onSelect}
      aria-pressed={selected}
      className={`w-full flex items-center justify-between gap-4 text-left px-5 py-3 rounded-2xl border-2 transition-[color,background-color,border-color,transform] active:scale-[0.98] ${
        selected
          ? "border-neutral-900 bg-neutral-900 text-white"
          : "border-neutral-200 bg-white text-neutral-900 hover:bg-neutral-50"
      }`}
    >
      <div className="min-w-0">
        <div className="text-lg font-semibold truncate">
          <DrinkName drink={order.drink} beans={order.beans} />
        </div>
        <div
          className={`text-sm truncate ${selected ? "text-neutral-300" : "text-neutral-500"}`}
        >
          {order.customerName || "Anonymous"}
        </div>
      </div>
      <div className="shrink-0 text-right text-sm">
        <div
          className={
            order.status === "making"
              ? selected
                ? "text-amber-300"
                : "text-amber-700"
              : order.status === "failed"
                ? selected
                  ? "text-red-300"
                  : "text-red-600"
                : selected
                  ? "text-neutral-300"
                  : "text-neutral-500"
          }
        >
          {STATUS_LABEL[order.status]}
        </div>
        {order.status !== "making" && (
          <div className="text-neutral-400">
            {timeAgo(order.at)}
          </div>
        )}
      </div>
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
  const [fromQueue, setFromQueue] = useState<RecentOrder[]>([]);
  const [history, setHistory] = useState<RecentOrder[] | null>(null);
  const [historyError, setHistoryError] = useState(false);
  const [orderId, setOrderId] = useState<string | null>(null);
  const [customerFilter, setCustomerFilter] = useState("");
  const [issue, setIssue] = useState<string | null>(null);
  const [description, setDescription] = useState("");
  const [severity, setSeverity] = useState<Severity | null>(null);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [done, setDone] = useState(false);

  useEffect(() => {
    if (!viamConn) return;
    let cancelled = false;
    const loadQueue = async () => {
      try {
        const q = await getQueue(viamConn);
        if (!cancelled) setFromQueue(queueOrders(q));
      } catch (e) {
        console.error("failed to load the queue:", e);
      }
    };
    const loadHistory = async () => {
      try {
        const h = await getRecentOrders(viamConn);
        if (cancelled) return;
        setHistory(h);
        setHistoryError(false);
      } catch (e) {
        console.error("failed to load recent orders:", e);
        if (!cancelled) {
          setHistory((h) => h ?? []);
          setHistoryError(true);
        }
      }
    };
    loadQueue();
    loadHistory();
    const q = setInterval(loadQueue, QUEUE_POLL_MS);
    const h = setInterval(loadHistory, HISTORY_POLL_MS);
    return () => {
      cancelled = true;
      clearInterval(q);
      clearInterval(h);
    };
  }, [viamConn]);

  useEffect(() => {
    if (!done) return;
    const t = setTimeout(onBack, THANKS_DISMISS_MS);
    return () => clearTimeout(t);
  }, [done, onBack]);

  // The synced copy wins, since it knows the outcome; the queue fills in the
  // order being made and any finished too recently to have synced.
  const synced = new Set((history ?? []).map((o) => o.id));
  const orders: RecentOrder[] = [
    ...fromQueue.filter((o) => !synced.has(o.id)),
    ...(history ?? []),
  ];
  const needle = customerFilter.trim().toLowerCase();
  // The selected order stays listed so a filter can't hide what will be sent.
  const shownOrders = orders.filter(
    (o) =>
      needle === "" ||
      o.id === orderId ||
      o.customerName.toLowerCase().includes(needle),
  );

  const isOther = issue === OTHER_ISSUE;
  const canSubmit =
    connected &&
    !submitting &&
    orderId !== null &&
    issue !== null &&
    (!isOther || (description.trim() !== "" && severity !== null));

  async function submit() {
    if (!viamConn || !canSubmit || !orderId || !issue) return;
    setSubmitting(true);
    setError(null);
    try {
      const notes = description.trim();
      await reportIssue(viamConn, {
        orderId,
        issue,
        ...(notes && { description: notes }),
        ...(isOther && { severity: severity! }),
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
          {orders.length > 0 && (
            <input
              type="search"
              value={customerFilter}
              onChange={(e) => setCustomerFilter(e.target.value)}
              placeholder="Filter by customer name"
              aria-label="Filter orders by customer name"
              className="w-full px-5 py-3 bg-neutral-50 border-2 border-neutral-200 rounded-2xl text-neutral-900 text-base outline-none focus:border-neutral-400 transition-colors font-sans"
            />
          )}
          {history === null && orders.length === 0 ? (
            <p className="text-neutral-500">Loading orders…</p>
          ) : orders.length === 0 ? (
            <p className="text-neutral-500">
              {historyError
                ? "Couldn't load recent orders."
                : `There is no order from the last ${RECENT_ORDERS_DAYS} days to report on.`}
            </p>
          ) : shownOrders.length === 0 ? (
            <p className="text-neutral-500">
              No order from the last {RECENT_ORDERS_DAYS} days matches that name.
            </p>
          ) : (
            <div className="flex flex-col gap-2 max-h-[22rem] overflow-y-auto overflow-x-hidden">
              {shownOrders.map((o) => (
                <OrderRow
                  key={o.id}
                  order={o}
                  selected={orderId === o.id}
                  onSelect={() => setOrderId(o.id)}
                />
              ))}
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

        {issue !== null && (
          <section className="anim-in flex flex-col gap-3">
            <h2 className="text-sm font-mono uppercase tracking-widest text-neutral-500">
              3 · {isOther ? "Tell us more" : "Anything to add? (optional)"}
            </h2>
            <textarea
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              maxLength={MAX_DESCRIPTION}
              rows={3}
              autoFocus={isOther}
              placeholder={isOther ? "What went wrong?" : "Add a note"}
              className="w-full px-5 py-3 bg-neutral-50 border-2 border-neutral-200 rounded-2xl text-neutral-900 text-base outline-none focus:border-neutral-400 transition-colors font-sans resize-none"
            />
            {isOther && (
              <div className="flex flex-wrap items-center gap-2">
                <span className="text-sm text-neutral-500 mr-1">
                  How bad was it?
                </span>
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
            )}
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
