"use client";

import Link from "next/link";
import { type Machine } from "./data";
import { type QueueStatus } from "../lib/viamClient";
import { drinkLabel } from "../order/drinks";

function renderQueueStatus(queue: QueueStatus | null | undefined): string {
  if (queue === undefined) return "loading queue…";
  if (queue === null) return "queue unavailable";
  const pending = queue.orders.filter((o) => o.completed_at === "");
  const current = queue.is_busy && pending.length > 0 ? pending[0] : null;
  const waiting = current ? pending.length - 1 : pending.length;
  if (!current && waiting === 0) return "idle";
  const parts: string[] = [];
  if (current) {
    const step = current.raw_step || queue.current_step;
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
  queue: QueueStatus | null | undefined;
}) {
  const row = (
    <>
      <span
        title={m.online ? "Online" : "Offline"}
        className={`inline-block w-2 h-2 rounded-full mr-2 align-middle ${
          m.online ? "bg-green-500" : "bg-neutral-400"
        }`}
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
