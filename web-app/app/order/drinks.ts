export interface Drink {
  id: string;
  label: string;
  description: string;
  image: string;
  available: boolean;
}

export function drinkLabel(drinkId: string): string {
  if (!drinkId) return "";
  const found = DRINKS.find((d) => d.id === drinkId);
  if (found) return found.label;
  return drinkId.replace(/_/g, " ").replace(/\b\w/g, (c) => c.toUpperCase());
}

// Base drinks shown in the selection grid, in display order. Decaf is no longer
// its own card — it's a side toggle (see applyDecaf).
export const GRID_DRINK_IDS = [
  "espresso",
  "lungo",
  "americano",
  "iced_coffee",
  "latte",
  "cappuccino",
] as const;

// decaf has a real brew path only for espresso/lungo (see isDecafDrink in
// espresso.go). Every other drink ignores the toggle.
const DECAF_VARIANT: Record<string, string> = {
  espresso: "decaf",
  lungo: "decaf_lungo",
};

// applyDecaf maps a base grid drink to its decaf variant when the toggle is on.
export function applyDecaf(baseId: string, decaf: boolean): string {
  if (!decaf) return baseId;
  return DECAF_VARIANT[baseId] ?? baseId;
}

// baseDrinkId strips a decaf variant back to its grid card id.
export function baseDrinkId(drinkId: string): string {
  if (drinkId === "decaf") return "espresso";
  if (drinkId === "decaf_lungo") return "lungo";
  return drinkId;
}

export function isDecafId(drinkId: string): boolean {
  return drinkId === "decaf" || drinkId === "decaf_lungo";
}

export const DRINKS: Drink[] = [
  {
    id: "espresso",
    label: "Espresso",
    description: "Pure, bold, classic",
    image: "./espresso.png",
    available: true,
  },
  {
    id: "lungo",
    label: "Lungo",
    description: "Long pull, smooth finish",
    image: "./espresso.png",
    available: true,
  },
  {
    id: "decaf",
    label: "Decaf",
    description: "All the flavor, none of the buzz",
    image: "./espresso.png",
    available: true,
  },
  {
    id: "decaf_lungo",
    label: "Lungo",
    description: "Long pull, decaf style",
    image: "./espresso.png",
    available: true,
  },
  {
    id: "iced_coffee",
    label: "Iced Coffee",
    description: "Espresso + ice",
    image: "./iced-coffee.png",
    available: true,
  },
  {
    id: "americano",
    label: "Americano",
    description: "Espresso + hot water",
    image: "./americano.png",
    available: true,
  },
  {
    id: "latte",
    label: "Latte",
    description: "Espresso + steamed milk",
    image: "./latte.png",
    available: true,
  },
  {
    id: "cappuccino",
    label: "Cappuccino",
    description: "Espresso + foam + milk",
    image: "./cappuccino.png",
    available: true,
  },
];

export const GRID_DRINKS: Drink[] = GRID_DRINK_IDS.map(
  (id) => DRINKS.find((d) => d.id === id)!,
);
