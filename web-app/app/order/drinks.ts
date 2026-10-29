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
  return titleCase(drinkId);
}

// Base drinks shown in the selection grid, in display order. The beans are
// chosen separately (see ChooseDrink's bean picker).
export const GRID_DRINK_IDS = [
  "espresso",
  "lungo",
  "americano",
  "iced_coffee",
  "iced_latte",
  "latte",
  "cappuccino",
] as const;

export const REGULAR_BEANS = "regular";

// Display names for the beans a machine can grind (its get_queue bean_options).
// Unknown names fall back to a title-cased version, so a new grinder only
// needs a label here to read nicely.
const BEAN_LABELS: Record<string, string> = {
  regular: "Regular",
  decaf: "Decaf",
  light_roast: "Light Roast",
};

export function beansLabel(beans: string): string {
  return BEAN_LABELS[beans] ?? titleCase(beans);
}

function titleCase(id: string): string {
  return id.replace(/_/g, " ").replace(/\b\w/g, (c) => c.toUpperCase());
}

// The beans a machine offers, regular first. Machines running a module from
// before bean_options only report can_serve_decaf.
export function beanOptionsOf(q: {
  bean_options?: string[];
  can_serve_decaf?: boolean;
}): string[] {
  if (q.bean_options?.length) {
    return [
      REGULAR_BEANS,
      ...q.bean_options.filter((b) => b !== REGULAR_BEANS),
    ];
  }
  return q.can_serve_decaf ? [REGULAR_BEANS, "decaf"] : [REGULAR_BEANS];
}

// Decaf used to be its own drink. The backend still reports decaf espresso and
// lungo under those legacy ids (get_queue, get_usual), next to beans.
const LEGACY_DECAF: Record<string, string> = {
  decaf: "espresso",
  decaf_lungo: "lungo",
};

export interface DrinkChoice {
  drink: string;
  beans: string;
}

// splitDrink reads a drink id that may be a legacy decaf id into its base
// drink and beans. beans, when given, wins for non-legacy ids.
export function splitDrink(drink: string, beans?: string): DrinkChoice {
  const base = LEGACY_DECAF[drink];
  if (base) return { drink: base, beans: "decaf" };
  return { drink, beans: beans || REGULAR_BEANS };
}

// orderLabel names a drink with its beans, e.g. "Light Roast Lungo"; regular
// beans go unmentioned.
export function orderLabel(drink: string, beans?: string): string {
  const c = splitDrink(drink, beans);
  const label = drinkLabel(c.drink);
  if (!label || c.beans === REGULAR_BEANS) return label;
  return `${beansLabel(c.beans)} ${label}`;
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
    id: "iced_coffee",
    label: "Iced Coffee",
    description: "Espresso + ice",
    image: "./iced-coffee.png",
    available: true,
  },
  {
    id: "iced_latte",
    label: "Iced Latte",
    description: "Espresso + ice + cold milk",
    image: "./iced-latte.png",
    available: true,
  },
  {
    id: "americano",
    label: "Americano",
    description: "Espresso + hot water",
    image: "./americano.png",
    available: false,
  },
  {
    id: "latte",
    label: "Latte",
    description: "Espresso + steamed milk",
    image: "./latte.png",
    available: false,
  },
  {
    id: "cappuccino",
    label: "Cappuccino",
    description: "Espresso + foam + milk",
    image: "./cappuccino.png",
    available: false,
  },
];

const ALL_GRID_DRINKS: Drink[] = GRID_DRINK_IDS.map(
  (id) => DRINKS.find((d) => d.id === id)!,
);

export const GRID_DRINKS: Drink[] = ALL_GRID_DRINKS.filter((d) => d.available);

// Shown greyed out under "Coming soon": the machine has no brew path for these
// yet, so they can't be ordered.
export const COMING_SOON_DRINKS: Drink[] = ALL_GRID_DRINKS.filter(
  (d) => !d.available,
);
