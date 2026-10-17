"use client";

import Image from "next/image";
import { useState } from "react";
import {
  COMING_SOON_DRINKS,
  GRID_DRINKS,
  applyDecaf,
  baseDrinkId,
  hasMilkChoice,
  isDecafId,
  milkLabel,
} from "./drinks";
import type { Fulfillment } from "../lib/viamClient";

export function ChooseDrink({
  selectedDrink,
  fulfillment,
  milk,
  milkOptions,
  connected,
  onSelect,
  onFulfillmentChange,
  onMilkChange,
  onBack,
  onNext,
}: {
  selectedDrink: string | null;
  fulfillment: Fulfillment;
  milk: string;
  /** The machine's milk_options; the choice shows only when there are two or more. */
  milkOptions: string[];
  connected: boolean;
  onSelect: (id: string) => void;
  onFulfillmentChange: (f: Fulfillment) => void;
  onMilkChange: (milk: string) => void;
  onBack: () => void;
  onNext: () => void;
}) {
  const [decaf, setDecaf] = useState(isDecafId(selectedDrink ?? ""));
  const [unavailableTapped, setUnavailableTapped] = useState<string | null>(
    null,
  );
  const selectedBase = selectedDrink ? baseDrinkId(selectedDrink) : null;

  const handleDecaf = (next: boolean) => {
    setDecaf(next);
    if (selectedBase) onSelect(applyDecaf(selectedBase, next));
  };

  const renderDrinkCard = (drink: (typeof GRID_DRINKS)[number], i: number) => {
    const isSelected = selectedBase === drink.id;
    // The milk choice sits on the card itself once it's picked. It's a sibling
    // of the card button, not inside it, since buttons can't nest.
    const showMilk =
      isSelected && hasMilkChoice(drink.id) && milkOptions.length > 1;
    return (
      <div
        key={drink.id}
        style={{ animationDelay: `${150 + i * 100}ms` }}
        className={`anim-in relative transition-transform duration-150 ${
          isSelected ? "scale-[1.02]" : "scale-100"
        }`}
      >
        <button
          onClick={() => {
            setUnavailableTapped(null);
            onSelect(applyDecaf(drink.id, decaf));
          }}
          className={`drink-card relative w-full h-full flex flex-col items-center justify-center gap-1 py-4 rounded-2xl transition-[background-color,border-color,transform] duration-150 ${
            isSelected
              ? "bg-[#ebebeb] border-2 border-black"
              : "bg-neutral-100 border-2 border-transparent"
          }`}
        >
          <div className="flex flex-col items-center gap-1 -translate-y-2">
            <Image
              src={drink.image}
              alt={drink.label}
              width={140}
              height={140}
              className="object-contain h-[min(140px,14vh)] w-auto"
            />
            <p className="font-sans font-medium text-base text-black leading-tight">
              {drink.label}
            </p>
          </div>
        </button>
        {showMilk && (
          <div
            role="radiogroup"
            aria-label="Which milk would you like?"
            className="anim-in absolute top-1/2 -translate-y-1/2 inset-x-2 flex rounded-full bg-white p-1 shadow-sm"
          >
            {milkOptions.map((m) => (
              <button
                key={m}
                type="button"
                role="radio"
                aria-checked={milk === m}
                onClick={() => onMilkChange(m)}
                className={`flex-1 py-1.5 rounded-full font-mono font-semibold text-xs uppercase tracking-wider transition-colors duration-150 ${
                  milk === m
                    ? "bg-black text-white"
                    : "text-neutral-500 hover:text-neutral-900"
                }`}
              >
                {milkLabel(m)}
              </button>
            ))}
          </div>
        )}
      </div>
    );
  };

  const renderComingSoonCard = (
    drink: (typeof COMING_SOON_DRINKS)[number],
    i: number,
  ) => (
    <button
      key={drink.id}
      type="button"
      aria-disabled="true"
      onClick={() => setUnavailableTapped(drink.label)}
      style={{ animationDelay: `${150 + i * 100}ms` }}
      className="anim-in relative w-full flex flex-col items-center justify-center gap-1 py-4 rounded-2xl bg-neutral-50 border-2 border-dashed border-neutral-200 cursor-not-allowed"
    >
      <div className="flex flex-col items-center gap-1 -translate-y-2 opacity-40 grayscale">
        <Image
          src={drink.image}
          alt={drink.label}
          width={140}
          height={140}
          className="object-contain h-[min(140px,14vh)] w-auto"
        />
        <p className="font-sans font-medium text-base text-black leading-tight">
          {drink.label}
        </p>
      </div>
    </button>
  );

  return (
    <main className="@container relative h-full bg-white flex flex-col overflow-y-auto font-sans">
      <header className="sticky top-0 z-10 w-full bg-white px-10 pt-6 pb-2">
        <button
          type="button"
          onClick={onBack}
          aria-label="Go back"
          className="anim-in h-11 w-11 rounded-full border border-neutral-200 bg-white text-neutral-900 transition-colors hover:bg-neutral-50 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-neutral-400"
          style={{ animationDelay: "80ms" }}
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
      </header>
      <div className="my-auto flex flex-col gap-6 w-full max-w-[900px] mx-auto px-10">
        <div className="anim-in flex flex-wrap items-center justify-between gap-x-4">
          <h1 className="text-3xl tracking-tight font-semibold text-[#0a0a0a] whitespace-nowrap">
            Choose your drink
          </h1>

          <button
            type="button"
            role="switch"
            aria-checked={decaf}
            onClick={() => handleDecaf(!decaf)}
            className="ml-auto flex items-center gap-3 p-4 transition-colors"
          >
            <svg
              aria-hidden="true"
              xmlns="http://www.w3.org/2000/svg"
              width="24"
              height="24"
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              strokeWidth="2"
              strokeLinecap="round"
              strokeLinejoin="round"
              className="h-5 w-5 text-neutral-400"
            >
              <path d="M20.985 12.486a9 9 0 1 1-9.473-9.472c.405-.022.617.46.402.803a6 6 0 0 0 8.268 8.268c.344-.215.825-.004.803.401" />
            </svg>
            <span className="font-mono font-semibold text-base text-black uppercase tracking-wider">
              Decaf
            </span>
            <span
              className={`relative h-7 w-12 rounded-full transition-colors duration-150 ${
                decaf ? "bg-black" : "bg-neutral-300"
              }`}
            >
              <span
                className={`absolute top-1 left-1 h-5 w-5 rounded-full bg-white transition-transform duration-150 ${
                  decaf ? "translate-x-5" : "translate-x-0"
                }`}
              />
            </span>
          </button>
        </div>

        <div className="grid gap-3 grid-cols-2 @md:grid-cols-3 @lg:grid-cols-4">
          {GRID_DRINKS.map((drink, i) => renderDrinkCard(drink, i))}
        </div>

        {COMING_SOON_DRINKS.length > 0 && (
          <section className="flex flex-col gap-3">
            <div className="anim-in flex flex-col gap-1">
              <h2 className="font-mono font-semibold text-sm text-neutral-500 uppercase tracking-wider">
                Coming soon
              </h2>
              <p className="text-neutral-500 text-sm" aria-live="polite">
                {unavailableTapped
                  ? `Sorry, we're unable to make a ${unavailableTapped} just yet. Please pick another drink above.`
                  : "We're sorry, we can't make these drinks just yet."}
              </p>
            </div>
            <div className="grid gap-3 grid-cols-2 @md:grid-cols-3 @lg:grid-cols-4">
              {COMING_SOON_DRINKS.map((drink, i) =>
                renderComingSoonCard(drink, GRID_DRINKS.length + i),
              )}
            </div>
          </section>
        )}

        {/* Sticky so the fulfillment choice and Next stay reachable when the
            grid overflows a short screen; the fade marks cards scrolling under it. */}
        <div className="sticky bottom-0 z-10 -mx-10 flex flex-col gap-6 bg-white px-10 pb-6 before:pointer-events-none before:absolute before:inset-x-0 before:bottom-full before:h-6 before:bg-linear-to-t before:from-white before:to-transparent">
          <div
            className="anim-in flex justify-center"
            style={{ animationDelay: "500ms" }}
          >
            <div
              role="radiogroup"
              aria-label="How would you like to receive your drink?"
              className="inline-flex rounded-full bg-neutral-100 p-1"
            >
              {(
                [
                  { id: "pickup", label: "Pickup" },
                  { id: "delivery", label: "Delivery" },
                ] as const
              ).map((mode) => (
                <button
                  key={mode.id}
                  type="button"
                  role="radio"
                  aria-checked={fulfillment === mode.id}
                  onClick={() => onFulfillmentChange(mode.id)}
                  className={`px-8 py-2.5 rounded-full font-mono font-semibold text-sm uppercase tracking-wider transition-colors duration-150 ${
                    fulfillment === mode.id
                      ? "bg-black text-white"
                      : "text-neutral-500 hover:text-neutral-900"
                  }`}
                >
                  {mode.label}
                </button>
              ))}
            </div>
          </div>

          {!connected && (
            <p className="anim-in text-neutral-500 text-center text-sm -mt-4">
              Waiting to reconnect to the machine…
            </p>
          )}
          <button
            onClick={onNext}
            disabled={!selectedDrink || !connected}
            className="anim-in press w-full py-4 text-base font-mono font-semibold uppercase tracking-wider bg-black text-white rounded-full hover:bg-neutral-800 transition-colors disabled:opacity-30"
            style={{ animationDelay: "600ms" }}
          >
            Next
          </button>
        </div>
      </div>
    </main>
  );
}
