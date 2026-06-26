"use client";

import Image from "next/image";
import { useState } from "react";
import { GRID_DRINKS, applyDecaf, baseDrinkId, isDecafId } from "./drinks";

export function ChooseDrink({
  selectedDrink,
  rejection,
  connected,
  onSelect,
  onBack,
  onNext,
}: {
  selectedDrink: string | null;
  rejection: string | null;
  connected: boolean;
  onSelect: (id: string) => void;
  onBack: () => void;
  onNext: () => void;
}) {
  const [decaf, setDecaf] = useState(isDecafId(selectedDrink ?? ""));
  const selectedBase = selectedDrink ? baseDrinkId(selectedDrink) : null;

  const handleDecaf = (next: boolean) => {
    setDecaf(next);
    if (selectedBase) onSelect(applyDecaf(selectedBase, next));
  };

  const renderDrinkCard = (drink: (typeof GRID_DRINKS)[number], i: number) => {
    const isSelected = selectedBase === drink.id;
    return (
      <button
        key={drink.id}
        onClick={() => onSelect(applyDecaf(drink.id, decaf))}
        style={{ animationDelay: `${150 + i * 100}ms` }}
        className={`anim-in drink-card relative w-full flex flex-col items-center justify-center gap-1 py-4 rounded-2xl transition-[background-color,border-color,transform] duration-150 ${
          isSelected
            ? "bg-[#ebebeb] border-2 border-black scale-[1.02]"
            : "bg-neutral-100 border-2 border-transparent scale-100"
        }`}
      >
        <div className="flex flex-col items-center gap-1">
          <Image
            src={drink.image}
            alt={drink.label}
            width={140}
            height={140}
            className="object-contain h-[min(140px,14vh)] w-auto"
          />
          <p className="font-mono font-semibold text-base text-black uppercase tracking-wider leading-tight">
            {drink.label}
          </p>
          <p className="font-sans font-medium text-sm text-black/60 leading-tight text-pretty">
            {drink.description}
          </p>
        </div>
      </button>
    );
  };

  return (
    <main className="relative h-full bg-white flex flex-col items-center overflow-y-auto p-6 font-sans">
      <button
        type="button"
        onClick={onBack}
        aria-label="Go back"
        className="anim-in absolute left-6 top-6 h-11 w-11 rounded-full border border-neutral-200 bg-white text-neutral-900 transition-colors hover:bg-neutral-50 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-neutral-400"
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
      <div className="my-auto flex flex-col gap-6 w-full max-w-[1200px]">
        <div className="anim-in flex items-center justify-between gap-4">
          <h1 className="text-4xl font-semibold text-[#0a0a0a]">
            Choose your drink
          </h1>

          <button
            type="button"
            role="switch"
            aria-checked={decaf}
            onClick={() => handleDecaf(!decaf)}
            className="flex items-center gap-3 p-4 transition-colors"
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

        <div className="grid gap-4 grid-cols-[repeat(auto-fit,minmax(300px,1fr))]">
          {GRID_DRINKS.map((drink, i) => renderDrinkCard(drink, i))}
        </div>

        {rejection && (
          <p className="anim-in text-neutral-500 text-center text-sm -mt-2">
            {rejection}
          </p>
        )}

        {!connected && !rejection && (
          <p className="anim-in text-neutral-500 text-center text-sm -mt-4">
            Waiting to reconnect to the machine…
          </p>
        )}

        <button
          onClick={onNext}
          disabled={!selectedDrink || !connected}
          className="anim-in press w-full py-4 text-base font-medium bg-black text-white rounded-full hover:bg-neutral-800 transition-colors disabled:opacity-30"
          style={{ animationDelay: "600ms" }}
        >
          Next
        </button>
      </div>
    </main>
  );
}
