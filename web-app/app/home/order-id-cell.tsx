"use client";

import { useState } from "react";

/** The order ID is a UUID; only its first block is readable at a glance. */
function shortOrderId(orderId: string): string {
  return orderId.split("-")[0] || orderId;
}

/** An order ID shortened to its first block, with a button copying all of it. */
export function OrderIdCell({ orderId }: { orderId: string }) {
  const [copied, setCopied] = useState(false);
  if (!orderId) return <span className="text-neutral-400">—</span>;
  return (
    <span className="inline-flex items-center gap-1.5">
      <span className="font-mono text-xs" title={orderId}>
        {shortOrderId(orderId)}
      </span>
      <button
        onClick={(e) => {
          // Rows can toggle on click; copying an ID is not that.
          e.stopPropagation();
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
