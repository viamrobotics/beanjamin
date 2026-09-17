"use client";

import { useEffect, useRef, useState } from "react";
import * as VIAM from "@viamrobotics/sdk";
import {
  type OrderRecord,
  type Panel,
  type SortKey,
  type SortDir,
  panelTitle,
  panelEmptyMsg,
  loadVideosForOrder,
  getBinarySignedUrl,
  countVideosForOrders,
} from "./data";
import { PlanPanel } from "./plan-panel";
import { drinkLabel } from "../order/drinks";

const ORDER_COLUMNS = [
  { key: "time", label: "Time", defaultDir: "desc" },
  { key: "customer", label: "Customer", defaultDir: "asc" },
  { key: "drink", label: "Drink", defaultDir: "asc" },
  { key: "duration", label: "Duration", defaultDir: "desc" },
  { key: "status", label: "Status", defaultDir: "asc" },
] as const;

// +1 order ID, +1 machine, +1 video — none of them sortable.
const TABLE_COL_COUNT = ORDER_COLUMNS.length + 3;

const TH_BASE =
  "sticky top-0 z-10 bg-neutral-50 px-2 py-1.5 text-left font-medium border-b border-neutral-200";

// A clip cannot exist until the video-store's trailing segment closes (~35s)
// and then syncs to the cloud, so a just-finished order legitimately has no
// clip yet. Calling that "no clip" makes a pending sync look like a failure.
const CLIP_PENDING_WINDOW_MS = 5 * 60 * 1000;

type VideoEntry =
  | { state: "loading" }
  | { state: "error"; message: string }
  | {
      state: "ready";
      items: { id: string; url: string; capturedAt: Date | null }[];
    };

function formatDuration(ms: number): string {
  if (ms <= 0) return "—";
  const totalSeconds = Math.round(ms / 1000);
  const minutes = Math.floor(totalSeconds / 60);
  const seconds = totalSeconds % 60;
  if (minutes === 0) return `${seconds}s`;
  return `${minutes}m ${seconds}s`;
}

/** The order ID is a UUID; only its first block is readable at a glance. */
function shortOrderId(orderId: string): string {
  return orderId.split("-")[0] || orderId;
}

function OrderIdCell({ orderId }: { orderId: string }) {
  const [copied, setCopied] = useState(false);
  if (!orderId) return <span className="text-neutral-400">—</span>;
  return (
    <span className="inline-flex items-center gap-1.5">
      <span className="font-mono text-xs" title={orderId}>
        {shortOrderId(orderId)}
      </span>
      <button
        onClick={() => {
          // Clipboard access can be denied (insecure origin, permissions);
          // the ID stays readable in the title attribute either way.
          navigator.clipboard.writeText(orderId).then(
            () => {
              setCopied(true);
              setTimeout(() => setCopied(false), 1200);
            },
            (e) => console.error("failed to copy order ID:", e)
          );
        }}
        className="text-neutral-400 hover:text-neutral-900 transition-colors"
        aria-label={`Copy full order ID ${orderId}`}
        title="Copy full order ID"
      >
        {copied ? "✓" : "⧉"}
      </button>
    </span>
  );
}

function StatusCell({ order }: { order: OrderRecord }) {
  if (order.ok) return <span className="text-green-700">OK</span>;
  const label = order.cancelled ? "Cancelled" : "Failed";
  const tone = order.cancelled ? "text-amber-700" : "text-red-700";
  return (
    <span className={tone} title={order.errorMessage}>
      {order.failedStep ? `${label} · ${order.failedStep}` : label}
    </span>
  );
}

function VideoCell({
  order,
  count,
  countedAt,
  isExpanded,
  onToggle,
}: {
  order: OrderRecord;
  count: number | undefined;
  /** When the clip count was taken — the reference point for "still syncing". */
  countedAt: number;
  isExpanded: boolean;
  onToggle: () => void;
}) {
  if (!order.orderId) return <>—</>;
  if (isExpanded) {
    return (
      <button onClick={onToggle} className="text-blue-600 hover:underline">
        Hide
      </button>
    );
  }
  if (count === undefined || count === null) {
    return <span className="text-neutral-400">▶ …</span>;
  }
  if (count === 0) {
    const pending =
      countedAt - order.endTime.getTime() < CLIP_PENDING_WINDOW_MS;
    return (
      <span className="text-neutral-400 italic">
        {pending ? "syncing…" : "no clip"}
      </span>
    );
  }
  return (
    <button onClick={onToggle} className="text-blue-600 hover:underline">
      ▶ Watch ({count})
    </button>
  );
}

function compareOrders(
  a: OrderRecord,
  b: OrderRecord,
  sort: { key: SortKey; dir: SortDir }
): number {
  const sign = sort.dir === "desc" ? -1 : 1;
  switch (sort.key) {
    case "time":
      return sign * (a.startTime.getTime() - b.startTime.getTime());
    case "customer":
      return sign * a.customerName.localeCompare(b.customerName);
    case "drink":
      return sign * drinkLabel(a.drink).localeCompare(drinkLabel(b.drink));
    case "duration":
      return sign * (a.durationMs - b.durationMs);
    case "status":
      return sign * (Number(a.ok) - Number(b.ok));
  }
}

function VideoExpansion({ entry }: { entry: VideoEntry | undefined }) {
  if (!entry || entry.state === "loading") {
    return <p className="text-neutral-500 m-0">Loading video…</p>;
  }
  if (entry.state === "error") {
    return <p className="text-red-500 m-0">Error: {entry.message}</p>;
  }
  if (entry.items.length === 0) {
    return (
      <p className="text-neutral-500 m-0">No clip available yet.</p>
    );
  }
  return (
    <div className="space-y-2">
      {entry.items.map((item) => (
        <video
          key={item.id}
          controls
          src={item.url}
          className="max-w-full rounded-md border border-neutral-200"
        />
      ))}
    </div>
  );
}

function OrderTable({
  orders,
  viamClient,
  videoCountByOrder,
  countedAt,
  machineNameById,
}: {
  orders: OrderRecord[];
  viamClient: VIAM.ViamClient | null;
  videoCountByOrder: Map<string, number>;
  countedAt: number;
  machineNameById: Map<string, string>;
}) {
  const [sort, setSort] = useState<{ key: SortKey; dir: SortDir }>({
    key: "time",
    dir: "desc",
  });
  const [expandedOrder, setExpandedOrder] = useState<string | null>(null);
  const [videoByOrder, setVideoByOrder] = useState<Map<string, VideoEntry>>(
    new Map()
  );

  const sorted = [...orders].sort((a, b) => compareOrders(a, b, sort));

  const toggleVideo = (order: OrderRecord) => {
    const orderId = order.orderId;
    if (expandedOrder === orderId) {
      setExpandedOrder(null);
      return;
    }
    setExpandedOrder(orderId);
    if (videoByOrder.has(orderId) || !viamClient) return;
    setVideoByOrder((prev) => new Map(prev).set(orderId, { state: "loading" }));
    (async () => {
      try {
        const videos = await loadVideosForOrder(viamClient, orderId);
        const items = await Promise.all(
          videos.map(async (v) => ({
            id: v.binaryDataId,
            url: await getBinarySignedUrl(viamClient, v.binaryDataId),
            capturedAt: v.capturedAt,
          }))
        );
        setVideoByOrder((prev) =>
          new Map(prev).set(orderId, { state: "ready", items })
        );
      } catch (e) {
        console.error("failed to load videos:", e);
        const message = e instanceof Error ? e.message : String(e);
        setVideoByOrder((prev) =>
          new Map(prev).set(orderId, { state: "error", message })
        );
      }
    })();
  };

  return (
    <>
      {/* Scrolls rather than paginating, so a whole day is one list. The
          header sticks so the columns stay readable partway down. */}
      <div className="max-h-[28rem] overflow-y-auto rounded-md border border-neutral-200 bg-white">
        <table className="w-full border-collapse text-sm">
          <thead>
            <tr className="text-left text-neutral-500">
              {ORDER_COLUMNS.map((col) => {
                const active = sort.key === col.key;
                const arrow = active ? (sort.dir === "desc" ? "↓" : "↑") : "";
                return (
                  <th
                    key={col.key}
                    className={`${TH_BASE} cursor-pointer select-none hover:text-neutral-900 transition-colors`}
                    onClick={() =>
                      setSort((s) =>
                        s.key === col.key
                          ? {
                              key: col.key,
                              dir: s.dir === "desc" ? "asc" : "desc",
                            }
                          : { key: col.key, dir: col.defaultDir }
                      )
                    }
                  >
                    {col.label} {arrow}
                  </th>
                );
              })}
              <th className={TH_BASE}>Order ID</th>
              <th className={TH_BASE}>Machine</th>
              <th className={TH_BASE}>Video</th>
            </tr>
          </thead>
          <tbody>
            {sorted.flatMap((o) => {
              const rowKey = o.orderId || o.startTime.toISOString();
              const isExpanded = !!o.orderId && expandedOrder === o.orderId;
              const rows = [
                <tr key={rowKey} className="border-t border-neutral-200">
                  <td className="px-2 py-1.5 whitespace-nowrap">
                    {o.startTime.toLocaleTimeString(undefined, {
                      hour: "numeric",
                      minute: "2-digit",
                    })}
                  </td>
                  <td className="px-2 py-1.5">{o.customerName || "—"}</td>
                  <td className="px-2 py-1.5">
                    {drinkLabel(o.drink) || "—"}
                    {o.decaf && (
                      <span className="ml-1.5 px-1.5 py-0.5 rounded-full bg-neutral-100 text-neutral-600 text-[10px] font-semibold tracking-wide">
                        DECAF
                      </span>
                    )}
                  </td>
                  <td className="px-2 py-1.5 tabular-nums">
                    {formatDuration(o.durationMs)}
                  </td>
                  <td className="px-2 py-1.5">
                    <StatusCell order={o} />
                  </td>
                  <td className="px-2 py-1.5">
                    <OrderIdCell orderId={o.orderId} />
                  </td>
                  <td className="px-2 py-1.5">
                    {machineNameById.get(o.robotId) ?? "—"}
                  </td>
                  <td className="px-2 py-1.5">
                    <VideoCell
                      order={o}
                      count={videoCountByOrder.get(o.orderId)}
                      countedAt={countedAt}
                      isExpanded={isExpanded}
                      onToggle={() => toggleVideo(o)}
                    />
                  </td>
                </tr>,
              ];
              if (isExpanded) {
                rows.push(
                  <tr
                    key={`${rowKey}-video`}
                    className="border-t border-neutral-200 bg-white"
                  >
                    <td colSpan={TABLE_COL_COUNT} className="px-2 py-3">
                      <div className="flex flex-col gap-3">
                        <VideoExpansion entry={videoByOrder.get(o.orderId)} />
                        <PlanPanel
                          orderId={o.orderId}
                          viamClient={viamClient}
                        />
                      </div>
                    </td>
                  </tr>
                );
              }
              return rows;
            })}
          </tbody>
        </table>
      </div>
      <p className="mt-2 text-sm text-neutral-500">{orders.length} orders</p>
    </>
  );
}

/** One line of context above the table: totals, and what went wrong. */
function summarize(orders: OrderRecord[]): string {
  const failed = orders.filter((o) => !o.ok && !o.cancelled).length;
  const cancelled = orders.filter((o) => o.cancelled).length;
  const parts = [`${orders.length} orders`];
  if (failed > 0) parts.push(`${failed} failed`);
  if (cancelled > 0) parts.push(`${cancelled} cancelled`);
  return parts.join(" · ");
}

export function OrdersPanel({
  panel,
  orders,
  error,
  onClose,
  viamClient,
  machineNameById,
}: {
  panel: Panel;
  orders: OrderRecord[] | null;
  error: string | null;
  onClose: () => void;
  viamClient: VIAM.ViamClient | null;
  machineNameById: Map<string, string>;
}) {
  const panelRef = useRef<HTMLDivElement>(null);
  // `at` is captured with the counts so a row can tell a clip that has not
  // synced yet from one that will never arrive, without reading the clock
  // during render.
  const [videoCounts, setVideoCounts] = useState<{
    counts: Map<string, number>;
    at: number;
  } | null>(null);
  const loaded = orders !== null || error !== null;
  const noTable = orders === null || orders.length === 0 || error !== null;
  const needsCounts = !!orders && orders.some((o) => !!o.orderId);
  const tableReady = !needsCounts || videoCounts !== null;
  const ready = loaded && (noTable || tableReady);

  useEffect(() => {
    if (!viamClient || !orders) return;
    const targets = orders.filter((o) => !!o.orderId).map((o) => o.orderId);
    if (targets.length === 0) return;
    let cancelled = false;
    countVideosForOrders(viamClient, targets)
      .then((counts) => {
        if (!cancelled) setVideoCounts({ counts, at: Date.now() });
      })
      .catch((e) => {
        console.error("failed to count videos:", e);
        if (!cancelled) setVideoCounts({ counts: new Map(), at: Date.now() });
      });
    return () => {
      cancelled = true;
    };
  }, [orders, viamClient]);

  // Scrolls once on mount (loading indicator visible) and once more when
  // content fully loads. `[ready]` fires on initial mount + ready flip.
  // The today panel is open before the user asks for anything, so scrolling
  // to it would drag the page down on every load.
  const autoScroll = panel.kind !== "today";
  useEffect(() => {
    if (!autoScroll) return;
    const raf = requestAnimationFrame(() => {
      panelRef.current?.scrollIntoView({ behavior: "smooth", block: "end" });
    });
    return () => cancelAnimationFrame(raf);
  }, [ready, autoScroll]);
  return (
    <div
      ref={panelRef}
      className="mt-4 p-4 border border-neutral-200 rounded-lg bg-neutral-50 scroll-mb-6"
    >
      <div className="flex justify-between items-center gap-3 mb-3">
        <strong className="text-neutral-900">{panelTitle(panel)}</strong>
        {orders && orders.length > 0 && (
          <span className="text-sm text-neutral-500 grow">
            {summarize(orders)}
          </span>
        )}
        <button
          onClick={onClose}
          className="border-none bg-transparent cursor-pointer text-neutral-500 hover:text-neutral-900 transition-colors text-lg leading-none"
          aria-label="Close"
        >
          ×
        </button>
      </div>
      {error ? (
        <p className="text-red-500">Error: {error}</p>
      ) : orders === null ? (
        <p className="text-neutral-500">Loading orders…</p>
      ) : orders.length === 0 ? (
        <p className="text-neutral-500">{panelEmptyMsg(panel)}</p>
      ) : !tableReady ? (
        <p className="text-neutral-500">Loading orders…</p>
      ) : (
        <OrderTable
          orders={orders}
          viamClient={viamClient}
          videoCountByOrder={videoCounts?.counts ?? new Map()}
          countedAt={videoCounts?.at ?? 0}
          machineNameById={machineNameById}
        />
      )}
    </div>
  );
}
