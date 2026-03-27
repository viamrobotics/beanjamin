"use client";

import Image from "next/image";
import { useEffect, useState, useRef } from "react";
import { getQueue, type ViamConnection } from "../lib/viamClient";

const STAGES = [
  { label: "Grinding...", image: "./progress-grinding.png", duration: 30000 },
  { label: "Tamping...", image: "./progress-pressing.png", duration: 15000 },
  { label: "Pulling...", image: "./progress-pulling.png", duration: 30000 },
] as const;

const PALETTES = [
  // Grinding — cool muted (teal, slate blue, lavender)
  ["#5B9EA6", "#7B8CBA", "#A78BBA"],
  // Tamping — mid warm (rose, magenta, violet)
  ["#C76B8A", "#9B59B6", "#E08DAC"],
  // Pulling — hot bright (amber, coral, gold)
  ["#E8913A", "#D94F4F", "#F5C242"],
];

const QUEUE_PALETTE = ["#8B7355", "#A0926B", "#6B5B45"];

interface ProgressProps {
  customerName: string;
  viamConn: ViamConnection | null;
  onComplete: () => void;
}

export function Progress({ customerName, viamConn, onComplete }: ProgressProps) {
  const [pickedUp, setPickedUp] = useState(false);
  const [stage, setStage] = useState(0);
  const [fillKey, setFillKey] = useState(0);
  const completeCalled = useRef(false);

  // Poll get_queue until the customer's order is at the front of the queue
  // (position 0), meaning it's the one currently being processed.
  // If the name isn't in the queue at all, it's already been processed.
  useEffect(() => {
    if (pickedUp || !viamConn) return;

    let cancelled = false;
    const poll = async () => {
      try {
        const q = await getQueue(viamConn);
        if (cancelled) return;
        const idx = q.orders.indexOf(customerName);
        // Front of queue (being processed) or not in queue at all (already done).
        if (idx === 0 || idx === -1) {
          setPickedUp(true);
        }
      } catch {
        // ignore polling errors
      }
    };

    // Check immediately, then every 2 seconds.
    poll();
    const interval = setInterval(poll, 2000);
    return () => {
      cancelled = true;
      clearInterval(interval);
    };
  }, [pickedUp, customerName, viamConn]);

  // Stage timer — only runs once the order has been picked up.
  useEffect(() => {
    if (!pickedUp) return;

    const timer = setTimeout(() => {
      if (stage < STAGES.length - 1) {
        setStage((s) => s + 1);
        setFillKey((k) => k + 1);
      } else if (!completeCalled.current) {
        completeCalled.current = true;
        onComplete();
      }
    }, STAGES[stage].duration);
    return () => clearTimeout(timer);
  }, [pickedUp, stage, onComplete]);

  const waiting = !pickedUp;
  const palette = waiting ? QUEUE_PALETTE : PALETTES[stage];
  const label = waiting ? "In queue..." : STAGES[stage].label;
  const imageSrc = waiting ? "./beans.png" : STAGES[stage].image;
  const imageAlt = waiting ? "Waiting in queue" : STAGES[stage].label;

  return (
    <main className="relative h-dvh overflow-hidden flex flex-col items-center justify-center">
      {/* Animated gradient background */}
      <div className="absolute inset-0 -z-10">
        <div
          className="absolute inset-[-50%] w-[200%] h-[200%] gradient-drift"
          style={{
            background: `
              radial-gradient(ellipse 60% 50% at 30% 40%, ${palette[0]}cc, transparent 70%),
              radial-gradient(ellipse 50% 60% at 70% 60%, ${palette[1]}bb, transparent 70%),
              radial-gradient(ellipse 70% 40% at 50% 30%, ${palette[2]}aa, transparent 70%)
            `,
            filter: "blur(80px)",
            transition: "background 1.5s ease",
          }}
        />
        {/* Noise overlay */}
        <svg className="absolute inset-0 w-full h-full opacity-[0.08] pointer-events-none mix-blend-overlay">
          <filter id="noise">
            <feTurbulence type="fractalNoise" baseFrequency="0.65" numOctaves="3" stitchTiles="stitch" />
          </filter>
          <rect width="100%" height="100%" filter="url(#noise)" />
        </svg>
      </div>

      {/* Content */}
      <div className="flex flex-col items-center gap-10 w-full max-w-md px-8">
        {/* Stage image with pop in/out */}
        <div className="relative w-[200px] h-[200px]">
          <div key={waiting ? "queue" : stage} className="absolute inset-0 stage-icon-pop">
            <Image
              src={imageSrc}
              alt={imageAlt}
              width={200}
              height={200}
              priority
              className="object-contain w-full h-full drop-shadow-lg"
            />
          </div>
        </div>

        {/* Status label */}
        <p
          key={waiting ? "queue" : stage}
          className="anim-in text-xl font-mono font-semibold text-white/90 tracking-wider uppercase drop-shadow-md"
        >
          {label}
        </p>

        {/* Notched progress bar — hidden while waiting in queue */}
        {!waiting && (
          <div className="flex gap-2 w-full">
            {STAGES.map((_, i) => (
              <div
                key={i}
                className="flex-1 h-2 rounded-full bg-white/20 overflow-hidden"
              >
                <div
                  key={i === stage ? fillKey : `done-${i}`}
                  className={`h-full rounded-full ${i < stage
                    ? "w-full bg-white/80"
                    : i === stage
                      ? "progress-fill bg-white/80"
                      : "w-0"
                    }`}
                  style={i === stage ? { animationDuration: `${STAGES[i].duration}ms` } : undefined}
                />
              </div>
            ))}
          </div>
        )}
      </div>
    </main>
  );
}
