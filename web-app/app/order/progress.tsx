"use client";

import Image from "next/image";
import { useEffect, useState, useRef } from "react";

const STAGES = [
  { label: "Grinding...", image: "/progress-grinding.png" },
  { label: "Tamping...", image: "/progress-pressing.png" },
  { label: "Pulling...", image: "/progress-pulling.png" },
] as const;

const STAGE_DURATION = 5000;

const PALETTES = [
  // Grinding — cool muted (teal, slate blue, lavender)
  ["#5B9EA6", "#7B8CBA", "#A78BBA"],
  // Tamping — mid warm (rose, magenta, violet)
  ["#C76B8A", "#9B59B6", "#E08DAC"],
  // Pulling — hot bright (amber, coral, gold)
  ["#E8913A", "#D94F4F", "#F5C242"],
];

export function Progress({ onComplete }: { onComplete: () => void }) {
  const [stage, setStage] = useState(0);
  const [fillKey, setFillKey] = useState(0);
  const completeCalled = useRef(false);

  useEffect(() => {
    const timer = setTimeout(() => {
      if (stage < STAGES.length - 1) {
        setStage((s) => s + 1);
        setFillKey((k) => k + 1);
      } else if (!completeCalled.current) {
        completeCalled.current = true;
        onComplete();
      }
    }, STAGE_DURATION);
    return () => clearTimeout(timer);
  }, [stage, onComplete]);

  const palette = PALETTES[stage];

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
          <div key={stage} className="absolute inset-0 stage-icon-pop">
            <Image
              src={STAGES[stage].image}
              alt={STAGES[stage].label}
              width={200}
              height={200}
              priority
              className="object-contain w-full h-full drop-shadow-lg"
            />
          </div>
        </div>

        {/* Status label */}
        <p
          key={stage}
          className="anim-in text-xl font-mono font-semibold text-white/90 tracking-wider uppercase drop-shadow-md"
        >
          {STAGES[stage].label}
        </p>

        {/* Notched progress bar */}
        <div className="flex gap-2 w-full">
          {STAGES.map((_, i) => (
            <div
              key={i}
              className="flex-1 h-2 rounded-full bg-white/20 overflow-hidden"
            >
              <div
                key={i === stage ? fillKey : `done-${i}`}
                className={`h-full rounded-full ${
                  i < stage
                    ? "w-full bg-white/80"
                    : i === stage
                      ? "progress-fill bg-white/80"
                      : "w-0"
                }`}
              />
            </div>
          ))}
        </div>
      </div>
    </main>
  );
}
