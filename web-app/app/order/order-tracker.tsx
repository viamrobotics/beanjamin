"use client";

import { useEffect, useState, useRef, useCallback } from "react";
import { getQueue, type ViamConnection, type QueueOrder } from "../lib/viamClient";

const DONE_DISPLAY_MS = 15_000;

// --- Step display rules -------------------------------------------------
//
// The Go module records the raw espresso step label per order. We show the
// raw label verbatim for prep/brew/serve so customers see what the robot is
// actually doing ("Grinding", "Tamping", "Brewing", ...). The only steps we
// hide are the cleanup steps that run after the espresso is in the cup —
// from the customer's perspective the drink is ready as soon as cleanup
// starts, so we collapse all cleanup labels into a single "Ready to pick up"
// state rendered with the same green card as the post-Dequeue confirmation.

const READY_RAW_STEPS = new Set<string>([
  "Grabbing filter",
  "Unlocking portafilter",
  "Cleaning",
  "Finishing up",
  "Ready", // terminal step set by the Go side after the espresso routine
]);

const READY_LABEL = "Ready to pick up";
const QUEUED_LABEL = "Making...";

function isReadyStep(rawStep: string): boolean {
  return READY_RAW_STEPS.has(rawStep);
}

function frontLabelFor(order: QueueOrder): string {
  if (!order.raw_step) return QUEUED_LABEL;
  return order.raw_step;
}

// Shared green card classes used by both the active "Ready to pick up" card
// and the post-Dequeue "Ready!" done card so the transition between them is
// visually seamless.
const GREEN_CARD_CLASSES =
  "anim-slide-in rounded-2xl bg-white border-2 border-emerald-300 px-5 py-4";
const NORMAL_CARD_CLASSES =
  "anim-slide-in rounded-2xl bg-white border border-neutral-200 px-5 py-4";

function ReadyCardBody({
  customerName,
  orderId,
  label,
}: {
  customerName: string;
  orderId: string;
  label: string;
}) {
  return (
    <>
      <div className="flex items-baseline justify-between gap-2">
        <p
          className="text-lg text-neutral-900 truncate"
          style={{ fontFamily: "var(--font-just-me), cursive" }}
        >
          {customerName}
        </p>
        <span className="text-[10px] font-mono text-neutral-300 shrink-0">
          {orderId.slice(0, 8)}
        </span>
      </div>
      <div className="flex items-center gap-2 mt-1">
        <span className="text-emerald-500 text-sm">&#10003;</span>
        <span className="text-xs font-mono font-medium text-emerald-600 uppercase tracking-wider">
          {label}
        </span>
      </div>
    </>
  );
}

interface DoneOrder {
  order: QueueOrder;
  removedAt: number;
}

interface OrderTrackerProps {
  viamConn: ViamConnection | null;
  onEmpty: () => void;
}

export function OrderTracker({ viamConn, onEmpty }: OrderTrackerProps) {
  const [queueOrders, setQueueOrders] = useState<QueueOrder[]>([]);
  const [doneOrders, setDoneOrders] = useState<DoneOrder[]>([]);
  const prevOrderIds = useRef<Set<string>>(new Set());
  const prevOrderMap = useRef<Map<string, QueueOrder>>(new Map());
  const doneIds = useRef<Set<string>>(new Set());
  // Don't fire onEmpty until we've seen at least one order — otherwise the
  // tracker dismisses itself immediately after mount because prepareOrder is
  // fired asynchronously from placeOrder and hasn't enqueued yet.
  const hasSeenOrders = useRef(false);

  const poll = useCallback(async () => {
    if (!viamConn) return;
    try {
      const q = await getQueue(viamConn);
      const current = q.orders;
      const currentIdSet = new Set(current.map((o) => o.id));

      // Detect orders that were in the previous poll but are gone now
      // Skip any we already moved to done (prevents duplicates)
      const newlyRemoved: QueueOrder[] = [];
      for (const id of prevOrderIds.current) {
        if (!currentIdSet.has(id) && !doneIds.current.has(id)) {
          const order = prevOrderMap.current.get(id);
          if (order) {
            newlyRemoved.push(order);
            doneIds.current.add(id);
          }
        }
      }

      if (newlyRemoved.length > 0) {
        setDoneOrders((prev) => [
          ...newlyRemoved.map((order) => ({ order, removedAt: Date.now() })),
          ...prev,
        ]);
      }

      prevOrderIds.current = currentIdSet;
      prevOrderMap.current = new Map(current.map((o) => [o.id, o]));
      setQueueOrders(current);
      if (current.length > 0) {
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

  // Clean up done orders after DONE_DISPLAY_MS
  useEffect(() => {
    if (doneOrders.length === 0) return;
    const timer = setInterval(() => {
      const now = Date.now();
      setDoneOrders((prev) => {
        const kept = prev.filter((o) => now - o.removedAt < DONE_DISPLAY_MS);
        // Clean up doneIds for expired entries
        const keptIds = new Set(kept.map((o) => o.order.id));
        for (const o of prev) {
          if (!keptIds.has(o.order.id)) {
            doneIds.current.delete(o.order.id);
          }
        }
        return kept;
      });
    }, 1000);
    return () => clearInterval(timer);
  }, [doneOrders.length]);

  // Notify parent when queue and done list are both empty — but only after
  // we've actually observed at least one order, so the tracker doesn't
  // dismiss itself during the brief window between mount and the enqueue
  // landing on the robot.
  useEffect(() => {
    if (
      hasSeenOrders.current &&
      queueOrders.length === 0 &&
      doneOrders.length === 0
    ) {
      onEmpty();
    }
  }, [queueOrders.length, doneOrders.length, onEmpty]);

  const hasOrders = queueOrders.length > 0 || doneOrders.length > 0;
  if (!hasOrders) return null;

  return (
    <div className="h-full flex flex-col bg-neutral-50 p-6">
      <h2 className="text-sm font-mono font-semibold text-neutral-400 uppercase tracking-widest mb-6">
        Orders
        <span className="ml-2 inline-flex items-center justify-center w-6 h-6 rounded-full bg-neutral-200 text-neutral-600 text-xs font-bold">
          {queueOrders.length}
        </span>
      </h2>

      <div className="flex-1 overflow-y-auto space-y-3">
        {/* Done orders at top — post-Dequeue confirmation */}
        {doneOrders.map((done) => (
          <div key={`done-${done.order.id}`} className={GREEN_CARD_CLASSES}>
            <ReadyCardBody
              customerName={done.order.customer_name}
              orderId={done.order.id}
              label="Ready!"
            />
          </div>
        ))}

        {/* Active queue orders */}
        {queueOrders.map((order, i) => {
          const isFront = i === 0;
          const isReady = isFront && isReadyStep(order.raw_step);

          if (isReady) {
            // Same green styling as the post-Dequeue card so the transition
            // from "Ready to pick up" → "Ready!" is just a label swap.
            return (
              <div key={order.id} className={GREEN_CARD_CLASSES}>
                <ReadyCardBody
                  customerName={order.customer_name}
                  orderId={order.id}
                  label={READY_LABEL}
                />
              </div>
            );
          }

          return (
            <div key={order.id} className={NORMAL_CARD_CLASSES}>
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
              {isFront ? (
                <div className="flex items-center gap-2 mt-1">
                  <span className="pulse-making inline-block w-2 h-2 rounded-full bg-amber-500" />
                  <span className="text-xs font-mono font-medium text-amber-600 uppercase tracking-wider">
                    {frontLabelFor(order)}
                  </span>
                </div>
              ) : (
                <p className="text-xs font-mono text-neutral-400 mt-1 uppercase tracking-wider">
                  In queue &middot; #{i + 1}
                </p>
              )}
            </div>
          );
        })}
      </div>
    </div>
  );
}
