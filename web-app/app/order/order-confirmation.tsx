"use client";

import Image from "next/image";
import { useState, useEffect, useRef } from "react";

const HEARTS = [
  { src: "./heart1.svg", width: 168, height: 180 },
  { src: "./heart2.svg", width: 149, height: 180 },
  { src: "./heart3.svg", width: 167, height: 180 },
  { src: "./heart4.svg", width: 161, height: 180 },
  { src: "./heart5.svg", width: 175, height: 180 },
];

interface OrderConfirmationProps {
  misspelled: string;
  actualName: string;
  drinkLabel: string;
  onDismiss: () => void;
}

export function OrderConfirmation({
  misspelled,
  actualName,
  drinkLabel,
  onDismiss,
}: OrderConfirmationProps) {
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  // Auto-dismiss after 4 seconds
  useEffect(() => {
    timerRef.current = setTimeout(onDismiss, 4000);
    return () => {
      if (timerRef.current) clearTimeout(timerRef.current);
    };
  }, [onDismiss]);

  const maxVw = 90;
  const charsPerLine = misspelled.length;
  const fontSize = `min(400px, ${maxVw / Math.max(charsPerLine * 0.55, 1)}vw)`;

  const [heart] = useState(() => {
    const pick = HEARTS[Math.floor(Math.random() * HEARTS.length)];
    return {
      ...pick,
      angle: Math.floor(Math.random() * 41) - 20,
      offsetY: Math.floor(Math.random() * 161) - 80,
    };
  });

  return (
    <main className="relative h-full bg-white flex flex-col items-center justify-center p-8 font-sans">
      <div className="flex flex-col items-center">
        <p className="anim-in text-neutral-400 text-lg uppercase tracking-widest font-mono">
          {drinkLabel} for
        </p>
        <div className="flex items-center gap-24">
          <div className="anim-scale" style={{ animationDelay: "150ms" }}>
            <p
              className="text-neutral-900 -mt-10 -rotate-5 whitespace-nowrap"
              style={{ fontFamily: "var(--font-just-me), cursive", fontSize }}
            >
              {misspelled}
            </p>
          </div>
          <div className="anim-fade" style={{ animationDelay: "400ms" }}>
            <Image
              src={heart.src}
              alt=""
              width={heart.width}
              height={heart.height}
              className="inline-block ml-2 align-middle"
              style={{ transform: `rotate(${heart.angle}deg) translateY(${heart.offsetY}px)` }}
            />
          </div>
        </div>
        <p
          className="anim-in text-neutral-400 uppercase tracking-widest font-mono text-md mb-8"
          style={{ animationDelay: "300ms" }}
        >
          (AKA {actualName})
        </p>
        <p
          className="anim-in text-neutral-500 text-sm text-center mb-10"
          style={{ animationDelay: "500ms" }}
        >
          Your order is in! Watch the board &rarr;
        </p>
        <button
          onClick={onDismiss}
          className="anim-in press px-16 py-4 text-base font-medium bg-neutral-900 text-white rounded-full transition-colors hover:bg-neutral-800"
          style={{ animationDelay: "700ms" }}
        >
          Place another order
        </button>
      </div>
    </main>
  );
}
