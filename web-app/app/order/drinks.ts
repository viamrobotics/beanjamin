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
  return drinkId
    .replace(/_/g, " ")
    .replace(/\b\w/g, (c) => c.toUpperCase());
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
    label: "Espresso Lungo",
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
    label: "Decaf Lungo",
    description: "Long pull, decaf style",
    image: "./espresso.png",
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
