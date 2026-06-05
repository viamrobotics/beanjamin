"use client";

import Link from "next/link";
import { type Machine } from "./data";
import { type MachineQueueState } from "../lib/viamClient";
import { drinkLabel } from "../order/drinks";

function renderQueueStatus(queue: MachineQueueState | undefined): string {
  if (queue === undefined) return "loading queue…";
  if (queue.kind === "error") return "queue unavailable";
  if (queue.kind === "no-service") return "no coffee service";
  const status = queue.status;
  const pending = status.orders.filter((o) => o.completed_at === "");
  const current = status.is_busy && pending.length > 0 ? pending[0] : null;
  const waiting = current ? pending.length - 1 : pending.length;
  if (!current && waiting === 0) return "idle";
  const parts: string[] = [];
  if (current) {
    const step = current.raw_step || status.current_step;
    const label = `making ${drinkLabel(current.drink) || current.drink} for ${current.customer_name || "?"}`;
    parts.push(step ? `${label} · ${step}` : label);
  }
  if (waiting > 0) parts.push(`${waiting} in queue`);
  return parts.join(" · ");
}

export function MachineRow({
  m,
  queue,
}: {
  m: Machine;
  queue: MachineQueueState | undefined;
}) {
  // Yellow = online and reachable but not running the coffee service.
  const noService = m.online && queue?.kind === "no-service";
  const dotClass = !m.online
    ? "bg-neutral-400"
    : noService
      ? "bg-yellow-500"
      : "bg-green-500";
  const dotTitle = !m.online
    ? "Offline"
    : noService
      ? "Online · no coffee service"
      : "Online";
  const row = (
    <>
      <span
        title={dotTitle}
        className={`inline-block w-2 h-2 rounded-full mr-2 align-middle ${dotClass}`}
      />
      <strong>{m.name}</strong>
      {m.online ? (
        <span className="text-neutral-500 ml-2">
          · {renderQueueStatus(queue)}
        </span>
      ) : (
        m.lastOnline && (
          <span className="text-neutral-500 ml-2">
            · last online {m.lastOnline.toLocaleString()}
          </span>
        )
      )}
    </>
  );
  if (!m.mainPartId) return <li>{row}</li>;
  return (
    <li>
      <Link
        href={`/machine?partId=${m.mainPartId}`}
        className="text-neutral-900 no-underline hover:underline"
      >
        {row}
      </Link>{" "}
      <Link
        href={`/machine?partId=${m.mainPartId}&kiosk=1`}
        className="text-blue-600 ml-2 hover:underline"
      >
        [kiosk mode →]
      </Link>
    </li>
  );
}
