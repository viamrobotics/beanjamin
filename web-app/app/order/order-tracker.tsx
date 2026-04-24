"use client";

import { useEffect, useState, useRef, useCallback } from "react";
import { getQueue, type ViamConnection, type QueueOrder } from "../lib/viamClient";

// --- Step display rules -------------------------------------------------
//
// The Go module records the raw espresso step label per order and owns
// the full lifecycle: pending → being made → cleanup → completed_at set →
// pruned after ~15s. The frontend just renders Status().orders in order.
//
// Render rules per order:
//   - completed_at non-empty       → green "Ready!" card
//   - raw_step in cleanup set      → green "Ready to pick up" card
//   - first pending, raw_step set  → amber dot + raw_step label
//   - first pending, no raw_step   → amber dot + "Making..."
//   - other pending                → "In queue · #N"

const READY_RAW_STEPS = new Set<string>([
  "Grabbing filter",
  "Unlocking portafilter",
  "Cleaning",
  "Finishing up",
]);

const READY_LABEL = "Ready to pick up";
const DONE_LABEL = "Ready!";
const QUEUED_LABEL = "Making...";

const GREEN_CARD_CLASSES =
  "anim-slide-in rounded-2xl bg-white border-2 border-emerald-300 px-5 py-4";
const NORMAL_CARD_CLASSES =
  "anim-slide-in rounded-2xl bg-white border border-neutral-200 px-5 py-4";

function isCompleted(order: QueueOrder): boolean {
  return Boolean(order.completed_at);
}

function isReadyDuringCleanup(order: QueueOrder): boolean {
  return READY_RAW_STEPS.has(order.raw_step);
}

interface OrderTrackerProps {
  viamConn: ViamConnection | null;
  onEmpty: () => void;
  // When true, stay mounted on an empty queue (and suppress onEmpty). Used by
  // the manual "View queue" takeover so it doesn't auto-dismiss.
  persistent?: boolean;
  // Optional × button in the header.
  onClose?: () => void;
}

export function OrderTracker({ viamConn, onEmpty, persistent, onClose }: OrderTrackerProps) {
  const [orders, setOrders] = useState<QueueOrder[]>([]);
  // Don't fire onEmpty until we've seen at least one order — otherwise the
  // tracker dismisses itself immediately after mount because prepareOrder is
  // fired asynchronously from placeOrder and hasn't enqueued yet.
  const hasSeenOrders = useRef(false);

  const poll = useCallback(async () => {
    if (!viamConn) return;
    try {
      const q = await getQueue(viamConn);
      setOrders(q.orders);
      if (q.orders.length > 0) {
        hasSeenOrders.current = true;
      }
    } catch (err) {
      console.error("[order-tracker] poll failed:", err);
    }
  }, [viamConn]);

  // Poll every second
  useEffect(() => {
    if (!viamConn) return;
    poll();
    const interval = setInterval(poll, 1000);
    return () => clearInterval(interval);
  }, [viamConn, poll]);

  // Notify parent when the backend has nothing left to show — but only for
  // order-initiated opens; persistent trackers stay up until closed.
  useEffect(() => {
    if (persistent) return;
    if (hasSeenOrders.current && orders.length === 0) {
      onEmpty();
    }
  }, [orders.length, onEmpty, persistent]);

  if (orders.length === 0 && !persistent) return null;

  // Pending count for the header chip — orders that haven't completed yet.
  const pendingCount = orders.filter((o) => !isCompleted(o)).length;
  // Index of the first pending order, used to decide which card gets the
  // amber "currently being made" treatment vs. "in queue".
  const firstPendingIdx = orders.findIndex((o) => !isCompleted(o));

  return (
    <div className="h-full flex flex-col bg-neutral-50 p-6">
      <div className="flex items-center justify-between mb-6">
        <h2 className="text-sm font-mono font-semibold text-neutral-400 uppercase tracking-widest">
          Orders
          <span className="ml-2 inline-flex items-center justify-center w-6 h-6 rounded-full bg-neutral-200 text-neutral-600 text-xs font-bold">
            {pendingCount}
          </span>
        </h2>
        {onClose && (
          <button
            onClick={onClose}
            aria-label="Close queue"
            className="text-neutral-400 hover:text-neutral-700 text-xl leading-none px-2"
          >
            ×
          </button>
        )}
      </div>

      {orders.length === 0 ? (
        <div className="flex-1 flex items-center justify-center">
          <p className="text-sm font-mono text-neutral-400 uppercase tracking-widest">
            No active orders
          </p>
        </div>
      ) : (
        <div className="flex-1 overflow-y-auto space-y-3">
          {orders.map((order, i) => {
            if (isCompleted(order)) {
              return (
                <OrderCard
                  key={order.id}
                  order={order}
                  cardClass={GREEN_CARD_CLASSES}
                  statusKind="ready"
                  label={DONE_LABEL}
                />
              );
            }
            if (isReadyDuringCleanup(order)) {
              return (
                <OrderCard
                  key={order.id}
                  order={order}
                  cardClass={GREEN_CARD_CLASSES}
                  statusKind="ready"
                  label={READY_LABEL}
                />
              );
            }
            if (i === firstPendingIdx) {
              return (
                <OrderCard
                  key={order.id}
                  order={order}
                  cardClass={NORMAL_CARD_CLASSES}
                  statusKind="making"
                  label={order.raw_step || QUEUED_LABEL}
                />
              );
            }
            // Other pending orders: position relative to the front pending order.
            const queuePosition = i - firstPendingIdx + 1;
            return (
              <OrderCard
                key={order.id}
                order={order}
                cardClass={NORMAL_CARD_CLASSES}
                statusKind="queued"
                label={`In queue · #${queuePosition}`}
              />
            );
          })}
        </div>
      )}
    </div>
  );
}

interface OrderCardProps {
  order: QueueOrder;
  cardClass: string;
  statusKind: "ready" | "making" | "queued";
  label: string;
}

function OrderCard({ order, cardClass, statusKind, label }: OrderCardProps) {
  return (
    <div className={cardClass}>
      <div className="flex items-baseline justify-between gap-2">
        <p
          className="text-lg text-neutral-900 truncate"
          style={{ fontFamily: "var(--font-just-me), cursive" }}
        >
          {order.customer_name}
        </p>
        <span className="text-[10px] font-mono text-neutral-300 shrink-0">
          {order.id.slice(0, 8)}
        </span>
      </div>
      {statusKind === "ready" ? (
        <div className="flex items-center gap-2 mt-1">
          <span className="text-emerald-500 text-sm">&#10003;</span>
          <span className="text-xs font-mono font-medium text-emerald-600 uppercase tracking-wider">
            {label}
          </span>
        </div>
      ) : statusKind === "making" ? (
        <div className="flex items-center gap-2 mt-1">
          <span className="pulse-making inline-block w-2 h-2 rounded-full bg-amber-500" />
          <span className="text-xs font-mono font-medium text-amber-600 uppercase tracking-wider">
            {label}
          </span>
        </div>
      ) : (
        <p className="text-xs font-mono text-neutral-400 mt-1 uppercase tracking-wider">
          {label}
        </p>
      )}
    </div>
  );
}
