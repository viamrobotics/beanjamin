import { test } from "node:test";
import assert from "node:assert/strict";
import {
  beanOptionsOf,
  orderLabel,
  orderParts,
  splitDrink,
  spokenOrder,
  unavailableDrinksOf,
} from "./drinks";

test("iced drinks are unavailable only while the ice machine is reported down", () => {
  const mug = { mug_drinks_available: true };
  assert.deepEqual(unavailableDrinksOf({ ...mug, iced_available: false }), [
    "iced_coffee",
    "iced_latte",
  ]);
  assert.deepEqual(unavailableDrinksOf({ ...mug, iced_available: true }), []);
  // Older modules, and machines without iced drinks, don't report it.
  assert.deepEqual(unavailableDrinksOf(mug), []);
  // The same array each poll, so the drink screen doesn't re-render.
  assert.equal(
    unavailableDrinksOf({ iced_available: false }),
    unavailableDrinksOf({ iced_available: false }),
  );
});

test("mug drinks are unavailable unless the machine reports them", () => {
  assert.deepEqual(unavailableDrinksOf({}), ["americano", "hot_water"]);
  assert.deepEqual(unavailableDrinksOf({ mug_drinks_available: false }), [
    "americano",
    "hot_water",
  ]);
  assert.deepEqual(unavailableDrinksOf({ iced_available: false }), [
    "iced_coffee",
    "iced_latte",
    "americano",
    "hot_water",
  ]);
  assert.equal(unavailableDrinksOf({}), unavailableDrinksOf({}));
});

test("bean options put regular first and fall back to can_serve_decaf", () => {
  assert.deepEqual(
    beanOptionsOf({ bean_options: ["regular", "light_roast", "decaf"] }),
    ["regular", "light_roast", "decaf"],
  );
  assert.deepEqual(beanOptionsOf({ bean_options: ["decaf", "regular"] }), [
    "regular",
    "decaf",
  ]);
  // Modules from before bean_options.
  assert.deepEqual(beanOptionsOf({ can_serve_decaf: true }), [
    "regular",
    "decaf",
  ]);
  assert.deepEqual(beanOptionsOf({}), ["regular"]);
});

// The backend still reports decaf espresso and lungo under the legacy ids.
test("legacy decaf ids split into a drink and beans", () => {
  assert.deepEqual(splitDrink("decaf"), { drink: "espresso", beans: "decaf" });
  assert.deepEqual(splitDrink("decaf_lungo", "decaf"), {
    drink: "lungo",
    beans: "decaf",
  });
  assert.deepEqual(splitDrink("lungo", "light_roast"), {
    drink: "lungo",
    beans: "light_roast",
  });
  assert.deepEqual(splitDrink("espresso"), {
    drink: "espresso",
    beans: "regular",
  });
});

test("order labels put non-regular beans after the drink", () => {
  assert.equal(orderLabel("espresso"), "Espresso");
  assert.equal(orderLabel("espresso", "regular"), "Espresso");
  assert.equal(orderLabel("lungo", "light_roast"), "Lungo, light roast");
  assert.equal(orderLabel("decaf_lungo", "decaf"), "Lungo, decaf");
  assert.equal(orderLabel("decaf"), "Espresso, decaf");
  assert.equal(
    orderLabel("iced_coffee", "dark_roast"),
    "Iced Coffee, dark roast",
  );
  assert.deepEqual(orderParts("lungo", "decaf"), {
    drink: "Lungo",
    beans: "decaf",
    milk: null,
    spice: null,
  });
  assert.deepEqual(orderParts("lungo"), {
    drink: "Lungo",
    beans: null,
    milk: null,
    spice: null,
  });
});

test("order labels name the milk only for a drink with a milk choice", () => {
  assert.equal(
    orderLabel("iced_latte", "regular", "oat"),
    "Iced Latte, oat milk",
  );
  assert.equal(
    orderLabel("iced_latte", "decaf", "oat"),
    "Iced Latte, decaf, oat milk",
  );
  assert.equal(orderLabel("iced_latte", "regular", ""), "Iced Latte");
  assert.equal(orderLabel("iced_coffee", "regular", "oat"), "Iced Coffee");
});

test("order labels name the spice only for a drink that takes one", () => {
  assert.equal(
    orderLabel("iced_latte", "regular", "oat", "cinnamon"),
    "Iced Latte, oat milk, cinnamon",
  );
  assert.equal(
    orderLabel("iced_latte", "regular", "oat", ""),
    "Iced Latte, oat milk",
  );
  assert.equal(orderLabel("espresso", "regular", "", "cinnamon"), "Espresso");
});

test("hot water is labeled without beans", () => {
  assert.equal(orderLabel("hot_water", "decaf"), "Hot Water");
  assert.equal(spokenOrder("hot_water", "decaf"), "hot water");
  assert.equal(orderLabel("americano", "decaf"), "Americano, decaf");
});

test("spoken orders read naturally", () => {
  assert.equal(spokenOrder("lungo", "light_roast"), "light roast lungo");
  assert.equal(spokenOrder("espresso"), "espresso");
  assert.equal(spokenOrder("decaf"), "decaf espresso");
  assert.equal(
    spokenOrder("iced_latte", "decaf", "oat"),
    "decaf iced latte with oat milk",
  );
  assert.equal(
    spokenOrder("iced_latte", "regular", "oat", "nutmeg"),
    "iced latte with oat milk and a dash of nutmeg",
  );
});
