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

// milkLabel names one of a machine's milk_options for display, e.g. "oat" ->
// "Oat". The milks come from the machine, so there is no table to look up.
export function milkLabel(milk: string): string {
  return titleCase(milk);
}

// hasMilkChoice reports whether the drink is made with a choice of milk (see
// IsMilk in coffee/order/drinks.go).
export function hasMilkChoice(drinkId: string): boolean {
  return drinkId === "iced_latte";
}

// spiceLabel names one of a machine's spice_options for display, e.g.
// "cinnamon" -> "Cinnamon".
export function spiceLabel(spice: string): string {
  return titleCase(spice);
}

// hasSpiceChoice reports whether the drink may be ordered with a dash of spice
// (see TakesSpice in coffee/order/drinks.go).
export function hasSpiceChoice(drinkId: string): boolean {
  return drinkId === "iced_latte";
}

// Base drinks shown in the selection grid, in display order. The beans are
// chosen separately (see ChooseDrink's bean picker).
export const GRID_DRINK_IDS = [
  "espresso",
  "lungo",
  "americano",
  "hot_water",
  "iced_coffee",
  "iced_latte",
  "latte",
  "cappuccino",
] as const;

export const REGULAR_BEANS = "regular";

// brewsCoffee reports whether the drink is made from beans (see BrewsCoffee in
// coffee/order/drinks.go). Hot water has no beans to choose.
export function brewsCoffee(drinkId: string): boolean {
  return drinkId !== "hot_water";
}

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

// The drinks made with ice (see IsIced in coffee/order/drinks.go).
export const ICED_DRINK_IDS: readonly string[] = ["iced_coffee", "iced_latte"];
// The drinks served in a mug (see IsMug in coffee/order/drinks.go).
const MUG_DRINK_IDS: readonly string[] = ["americano", "hot_water"];
const ICED_AND_MUG_DRINK_IDS: readonly string[] = [
  ...ICED_DRINK_IDS,
  ...MUG_DRINK_IDS,
];
const NO_DRINKS: readonly string[] = [];

// The menu drinks the machine can't make right now: the iced ones while its
// ice machine is unreachable, and the mug drinks unless it reports
// mug_drinks_available. A machine that doesn't report iced_available (older
// module, or no iced drinks) keeps its iced drinks; one that doesn't report
// mug_drinks_available has no mug drinks. The arrays are shared constants so
// the queue poll doesn't re-render the drink screen.
export function unavailableDrinksOf(q: {
  iced_available?: boolean;
  mug_drinks_available?: boolean;
}): readonly string[] {
  const iced = q.iced_available === false;
  const mug = q.mug_drinks_available !== true;
  if (iced && mug) return ICED_AND_MUG_DRINK_IDS;
  if (iced) return ICED_DRINK_IDS;
  if (mug) return MUG_DRINK_IDS;
  return NO_DRINKS;
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

// orderParts splits an order's display name into the drink and its
// lowercase qualifiers: the beans for a coffee from anything but regular, the milk for a
// drink with a milk choice, and the spice for a drink that takes one:
// "Iced Latte" + "light roast" + "oat milk" + "cinnamon".
// DrinkName renders them with the qualifiers set apart.
export function orderParts(
  drink: string,
  beans?: string,
  milk?: string,
  spice?: string,
): {
  drink: string;
  beans: string | null;
  milk: string | null;
  spice: string | null;
} {
  const c = splitDrink(drink, beans);
  return {
    drink: drinkLabel(c.drink),
    beans:
      c.beans === REGULAR_BEANS || !brewsCoffee(c.drink)
        ? null
        : beansLabel(c.beans).toLowerCase(),
    milk:
      milk && hasMilkChoice(c.drink)
        ? `${milkLabel(milk).toLowerCase()} milk`
        : null,
    spice:
      spice && hasSpiceChoice(c.drink) ? spiceLabel(spice).toLowerCase() : null,
  };
}

// orderQualifiers joins orderParts' qualifiers, e.g. "light roast, oat milk,
// cinnamon"; empty for regular beans, no milk and no spice.
export function orderQualifiers(p: {
  beans: string | null;
  milk: string | null;
  spice?: string | null;
}): string {
  return [p.beans, p.milk, p.spice].filter(Boolean).join(", ");
}

// orderLabel is orderParts as plain text, e.g. "Lungo, light roast" or
// "Iced Latte, oat milk, cinnamon"; regular beans go unmentioned.
export function orderLabel(
  drink: string,
  beans?: string,
  milk?: string,
  spice?: string,
): string {
  const p = orderParts(drink, beans, milk, spice);
  const q = orderQualifiers(p);
  if (!p.drink || !q) return p.drink;
  return `${p.drink}, ${q}`;
}

// spokenOrder names an order the way it is said: "decaf lungo", "iced latte
// with oat milk", "iced latte with oat milk and a dash of cinnamon".
export function spokenOrder(
  drink: string,
  beans?: string,
  milk?: string,
  spice?: string,
): string {
  const p = orderParts(drink, beans, milk, spice);
  const said = [p.beans, p.drink.toLowerCase()].filter(Boolean).join(" ");
  const extras = [p.milk, p.spice && `a dash of ${p.spice}`].filter(Boolean);
  return extras.length ? `${said} with ${extras.join(" and ")}` : said;
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
    available: true,
  },
  {
    id: "hot_water",
    label: "Hot Water",
    description: "Hot water in a mug",
    image: "./hot-water.png",
    available: true,
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
