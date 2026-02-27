import Image from "next/image";
import { DRINKS } from "./drinks";
import { QueueIndicator } from "./queue-indicator";

function ComingSoonBadge() {
  return (
    <div className="absolute inset-0 flex items-center justify-center pointer-events-none">
      <div className="-rotate-15">
        <div className="bg-yellow-50 border border-yellow-800/20 px-2.5 pb-1 pt-0.5 rounded shadow-xs">
          <span className="font-mono text-xs uppercase tracking-widest text-yellow-800/80">
            Coming soon
          </span>
        </div>
      </div>
    </div>
  );
}

export function ChooseDrink({
  selectedDrink,
  queueCount,
  onSelect,
  onNext,
}: {
  selectedDrink: string | null;
  queueCount: number;
  onSelect: (id: string) => void;
  onNext: () => void;
}) {
  return (
    <main className="relative h-dvh bg-white flex flex-col items-center justify-center p-8 font-sans">
      <QueueIndicator count={queueCount} />
      <div className="flex flex-col gap-10 w-full max-w-[512px]">
        <h1 className="anim-in text-2xl font-semibold text-[#0a0a0a] text-center">
          Choose your drink
        </h1>

        <div className="flex flex-wrap gap-4">
          {DRINKS.map((drink, i) => {
            const isSelected = selectedDrink === drink.id;
            return (
              <button
                key={drink.id}
                onClick={() => drink.available && onSelect(drink.id)}
                disabled={!drink.available}
                style={{ animationDelay: `${150 + i * 100}ms` }}
                className={`anim-in drink-card relative flex flex-col items-center justify-center gap-1 p-6 rounded-2xl transition-[background-color,border-color,transform] duration-150 basis-[calc(50%-8px)] ${
                  isSelected
                    ? "bg-[#ebebeb] border-2 border-black scale-[1.02]"
                    : "bg-neutral-100 border-2 border-transparent scale-100"
                } ${!drink.available ? "cursor-default" : ""}`}
              >
                <div className={!drink.available ? "opacity-50 flex flex-col items-center gap-1" : "flex flex-col items-center gap-1"}>
                  <Image
                    src={drink.image}
                    alt={drink.label}
                    width={140}
                    height={140}
                    className="object-contain"
                  />
                  <p className="font-mono font-semibold text-base text-black uppercase tracking-wider leading-tight">
                    {drink.label}
                  </p>
                  <p className="font-sans font-medium text-sm text-black/60 leading-tight">
                    {drink.description}
                  </p>
                </div>
                {!drink.available && <ComingSoonBadge />}
              </button>
            );
          })}
        </div>

        <button
          onClick={onNext}
          disabled={!selectedDrink}
          className="anim-in press w-full py-5 text-base font-medium bg-black text-white rounded-full hover:bg-neutral-800 transition-colors disabled:opacity-30"
          style={{ animationDelay: "600ms" }}
        >
          Next
        </button>
      </div>
    </main>
  );
}
