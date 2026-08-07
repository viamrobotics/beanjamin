import { test } from "node:test";
import assert from "node:assert/strict";
import { misspellName, containsProfanity, profanityWords } from "./misspell";

const NAMES = [
  "Mark",
  "Lisa",
  "Dave",
  "Greg",
  "Karen",
  "Alexander",
  "Priya",
  "Geoffrey",
  "Nguyen",
  "Bo",
  "Al",
  "Xu",
  "Elizabeth",
  "Chloe",
  "Steve",
  "Daniel",
  "Yvonne",
  "Siobhan",
];

const SEEDS = [0, 1, 2, 3, 4, 5, 6, 7, 8, 9];

test("always produces something different from the input", () => {
  for (const name of NAMES) {
    for (const seed of SEEDS) {
      const out = misspellName(name, seed);
      assert.notEqual(
        out.toLowerCase(),
        name.toLowerCase(),
        `${name} (seed ${seed}) came back unchanged`,
      );
      assert.ok(out.length > 0, `${name} (seed ${seed}) came back empty`);
    }
  }
});

test("is deterministic for a given seed", () => {
  for (const name of NAMES) {
    for (const seed of SEEDS) {
      assert.equal(misspellName(name, seed), misspellName(name, seed));
    }
  }
});

test("varies across seeds so repeat customers get different misspellings", () => {
  for (const name of NAMES) {
    const outputs = new Set(SEEDS.map((seed) => misspellName(name, seed)));
    assert.ok(
      outputs.size > 1,
      `${name} produced only "${[...outputs][0]}" across every seed`,
    );
  }
});

test("only mangles the first word", () => {
  for (const seed of SEEDS) {
    const out = misspellName("Mary Anne Smith", seed);
    assert.ok(
      out.endsWith(" Anne Smith"),
      `expected trailing words preserved, got "${out}"`,
    );
  }
});

test("never shortens names of three letters or fewer", () => {
  for (const name of ["Bo", "Al", "Xu", "Ann", "Ted"]) {
    for (const seed of SEEDS) {
      const out = misspellName(name, seed);
      assert.ok(
        out.length >= name.length,
        `${name} (seed ${seed}) shrank to "${out}"`,
      );
    }
  }
});

test("preserves the input's capitalization style", () => {
  for (const seed of SEEDS) {
    const capitalized = misspellName("Michael", seed);
    assert.equal(capitalized, capitalized.charAt(0).toUpperCase() + capitalized.slice(1));
    assert.notEqual(capitalized, capitalized.toUpperCase());

    assert.equal(misspellName("MICHAEL", seed), misspellName("MICHAEL", seed).toUpperCase());
  }
});

// Guards the runtime harvest in misspell.ts: if an obscenity upgrade changes
// the phrase-metadata shape, the substring layer would silently empty out
// instead of failing.
test("harvests a usable wordlist from the obscenity dataset", () => {
  assert.ok(
    profanityWords.length > 20,
    `expected a populated wordlist, got ${profanityWords.length} entries`,
  );
  assert.ok(profanityWords.every((w) => w.length >= 4 && /^[a-z]+$/.test(w)));
});

test("does not introduce profanity that wasn't in the name", () => {
  // Names chosen to sit one mutation away from something unfortunate.
  const risky = [
    ...NAMES,
    "Fiona",
    "Dickon",
    "Shipra",
    "Cassius",
    "Focus",
    "Pisces",
    "Raphael",
    "Wharton",
    "Analiese",
    "Titus",
  ];
  for (const name of risky) {
    for (const seed of SEEDS) {
      const out = misspellName(name, seed);
      if (containsProfanity(out) && !containsProfanity(name)) {
        assert.fail(`${name} (seed ${seed}) became "${out}"`);
      }
    }
  }
});

test("returns blank input untouched rather than inventing a name", () => {
  assert.equal(misspellName(""), "");
  assert.equal(misspellName("   "), "   ");
});

test("caps absurdly long input at 50 characters of source", () => {
  const long = "A".repeat(200);
  const out = misspellName(long, 3);
  assert.ok(out.length <= 52, `expected a bounded result, got ${out.length} chars`);
});
