"use client";

import Image from "next/image";
import { useState } from "react";
import {
  COMING_SOON_DRINKS,
  GRID_DRINKS,
  beansLabel,
  hasMilkChoice,
  hasSpiceChoice,
  milkLabel,
  spiceLabel,
} from "./drinks";
import type { Fulfillment } from "../lib/viamClient";

export function ChooseDrink({
  selectedDrink,
  beanOptions,
  selectedBeans,
  fulfillment,
  milk,
  milkOptions,
  waitLabel,
  unavailableDrinks,
  spice,
  spiceOptions,
  connected,
  onSelect,
  onBeansChange,
  onFulfillmentChange,
  onMilkChange,
  onSpiceChange,
  onBack,
  onNext,
}: {
  selectedDrink: string | null;
  /** The machine's beans, regular first; the picker hides with only one. */
  beanOptions: string[];
  selectedBeans: string;
  fulfillment: Fulfillment;
  milk: string;
  /** The machine's milk_options; the choice shows only when there are two or more. */
  milkOptions: string[];
  /** The expected wait for a new order, phrased; null hides it. */
  waitLabel: string | null;
  /** Menu drinks the machine can't make right now; shown greyed out in place. */
  unavailableDrinks: readonly string[];
  /** The chosen spice; "" for none. */
  spice: string;
  /** The machine's spice_options; the choice is hidden when it is empty. */
  spiceOptions: string[];
  connected: boolean;
  onSelect: (id: string) => void;
  onBeansChange: (beans: string) => void;
  onFulfillmentChange: (f: Fulfillment) => void;
  onMilkChange: (milk: string) => void;
  onSpiceChange: (spice: string) => void;
  onBack: () => void;
  onNext: () => void;
}) {
  const [unavailableTapped, setUnavailableTapped] = useState<string | null>(
    null,
  );
  const renderDrinkCard = (drink: (typeof GRID_DRINKS)[number], i: number) => {
    if (unavailableDrinks.includes(drink.id)) {
      return renderPausedCard(drink, i);
    }
    const isSelected = selectedDrink === drink.id;
    // The milk and spice choices sit on the card itself once it's picked.
    // They're siblings of the card button, not inside it, since buttons can't
    // nest.
    const showMilk =
      isSelected && hasMilkChoice(drink.id) && milkOptions.length > 1;
    const showSpice =
      isSelected && hasSpiceChoice(drink.id) && spiceOptions.length > 0;
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
            onSelect(drink.id);
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
        {(showMilk || showSpice) && (
          <div className="absolute top-1/2 -translate-y-1/2 inset-x-2 flex flex-col gap-1.5">
            {showMilk && (
              <OptionPills
                label="Which milk would you like?"
                options={milkOptions.map((m) => ({
                  value: m,
                  label: milkLabel(m),
                }))}
                selected={milk}
                onChange={onMilkChange}
              />
            )}
            {showSpice && (
              <OptionPills
                label="Would you like a dash of spice?"
                options={[
                  { value: "", label: "No spice" },
                  ...spiceOptions.map((s) => ({
                    value: s,
                    label: spiceLabel(s),
                  })),
                ]}
                selected={spice}
                onChange={onSpiceChange}
              />
            )}
          </div>
        )}
      </div>
    );
  };

  // A menu drink the machine can't make right now. It keeps its place in the
  // grid so the layout doesn't jump when the ice machine drops or comes back.
  const renderPausedCard = (
    drink: (typeof GRID_DRINKS)[number],
    i: number,
  ) => (
    <button
      key={drink.id}
      type="button"
      aria-disabled="true"
      aria-label={`${drink.label}, temporarily unavailable`}
      style={{ animationDelay: `${150 + i * 100}ms` }}
      className="anim-in relative w-full h-full flex flex-col items-center justify-center gap-1 py-4 rounded-2xl bg-neutral-50 border-2 border-transparent cursor-not-allowed"
    >
      <div className="flex flex-col items-center gap-1 -translate-y-2">
        <Image
          src={drink.image}
          alt=""
          width={140}
          height={140}
          className="object-contain h-[min(140px,14vh)] w-auto opacity-40 grayscale"
        />
        <p className="font-sans font-medium text-base text-neutral-400 leading-tight">
          {drink.label}
        </p>
        <p className="font-mono font-semibold text-[11px] uppercase tracking-wider text-neutral-500">
          Temporarily unavailable
        </p>
      </div>
    </button>
  );

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
          <div className="flex flex-col gap-1">
            <h1 className="text-3xl tracking-tight font-semibold text-[#0a0a0a] whitespace-nowrap">
              Choose your drink
            </h1>
            {waitLabel && (
              <p className="text-neutral-500 text-sm" aria-live="polite">
                {waitLabel}
              </p>
            )}
          </div>

          {beanOptions.length > 1 && (
            <div
              role="radiogroup"
              aria-label="Which beans?"
              className="ml-auto inline-flex rounded-full bg-neutral-100 p-1"
            >
              {beanOptions.map((beans) => (
                <button
                  key={beans}
                  type="button"
                  role="radio"
                  aria-checked={selectedBeans === beans}
                  onClick={() => onBeansChange(beans)}
                  className={`px-5 py-2.5 rounded-full font-mono font-semibold text-sm uppercase tracking-wider whitespace-nowrap transition-colors duration-150 ${
                    selectedBeans === beans
                      ? "bg-black text-white"
                      : "text-neutral-500 hover:text-neutral-900"
                  }`}
                >
                  {beansLabel(beans)}
                </button>
              ))}
            </div>
          )}
        </div>

        {GRID_DRINKS.some((d) => unavailableDrinks.includes(d.id)) && (
          <p
            className="anim-in -mt-3 text-neutral-500 text-sm"
            aria-live="polite"
          >
            Our ice machine is taking a break, so iced drinks are temporarily
            unavailable.
          </p>
        )}

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

// OptionPills is the row of radio pills a drink card shows for a choice that
// belongs to the drink (its milk, its spice).
function OptionPills({
  label,
  options,
  selected,
  onChange,
}: {
  label: string;
  options: { value: string; label: string }[];
  selected: string;
  onChange: (value: string) => void;
}) {
  return (
    <div
      role="radiogroup"
      aria-label={label}
      className="anim-in flex rounded-full bg-white p-1 shadow-sm"
    >
      {options.map((o) => (
        <button
          key={o.value}
          type="button"
          role="radio"
          aria-checked={selected === o.value}
          onClick={() => onChange(o.value)}
          className={`flex-1 py-1.5 rounded-full font-mono font-semibold text-xs uppercase tracking-wider whitespace-nowrap transition-colors duration-150 ${
            selected === o.value
              ? "bg-black text-white"
              : "text-neutral-500 hover:text-neutral-900"
          }`}
        >
          {o.label}
        </button>
      ))}
    </div>
  );
}
