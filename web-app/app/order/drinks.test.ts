import { test } from "node:test";
import assert from "node:assert/strict";
import {
  beanOptionsOf,
  orderLabel,
  orderParts,
  splitDrink,
  spokenOrder,
} from "./drinks";

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
  });
  assert.deepEqual(orderParts("lungo"), { drink: "Lungo", beans: null });
});

test("spoken orders read naturally", () => {
  assert.equal(spokenOrder("lungo", "light_roast"), "light roast lungo");
  assert.equal(spokenOrder("espresso"), "espresso");
  assert.equal(spokenOrder("decaf"), "decaf espresso");
});
