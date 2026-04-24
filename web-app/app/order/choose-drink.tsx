"use client";

import Image from "next/image";
import { DRINKS } from "./drinks";

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
  const firstRowDrinks = DRINKS.filter(
    (drink) => drink.id === "espresso" || drink.id === "lungo",
  );
  const decafDrinks = DRINKS.filter(
    (drink) => drink.id === "decaf" || drink.id === "decaf_lungo",
  );
  const secondRowDrinks = DRINKS.filter(
    (drink) =>
      drink.id === "americano" || drink.id === "cappuccino" || drink.id === "latte",
  );

  const renderDrinkCard = (drink: (typeof DRINKS)[number], i: number) => {
    const isSelected = selectedDrink === drink.id;
    return (
      <button
        key={drink.id}
        onClick={() => onSelect(drink.id)}
        style={{ animationDelay: `${150 + i * 100}ms` }}
        className={`anim-in drink-card relative w-full flex flex-col items-center justify-center gap-1 p-3 rounded-2xl transition-[background-color,border-color,transform] duration-150 ${
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
    <main className="relative h-full bg-white flex flex-col items-center justify-center p-4 font-sans">
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
      <div className="flex flex-col gap-4 w-full max-w-[720px]">
        <h1 className="anim-in text-2xl font-semibold text-[#0a0a0a] text-center">
          Choose your drink
        </h1>

        <div className="flex flex-col gap-3">
          <div className="grid grid-cols-2 gap-3">
            {firstRowDrinks.map((drink, i) => renderDrinkCard(drink, i))}
          </div>
          <div className="grid grid-cols-2 gap-3">
            {decafDrinks.map((drink, i) =>
              renderDrinkCard(drink, i + firstRowDrinks.length),
            )}
          </div>
          <div className="grid grid-cols-3 gap-3">
            {secondRowDrinks.map((drink, i) =>
              renderDrinkCard(drink, i + firstRowDrinks.length + decafDrinks.length),
            )}
          </div>
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
          className="anim-in press w-full py-3 text-base font-medium bg-black text-white rounded-full hover:bg-neutral-800 transition-colors disabled:opacity-30"
          style={{ animationDelay: "600ms" }}
        >
          Next
        </button>
      </div>
    </main>
  );
}
