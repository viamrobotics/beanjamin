/**
 * Deterministic barista-name misspeller: seven mechanical rules plus four
 * lookup tables, chosen by a seed.
 *
 * This replaced an LLM call. Nearly every strategy the prompt described was
 * plain string manipulation, and the seed already steered strategy choice —
 * so the model was doing rule dispatch over one short string, on the critical
 * path of the order, with an API key in the browser.
 */

import {
  DataSet,
  RegExpMatcher,
  englishDataset,
  englishRecommendedTransformers,
} from "obscenity";

const VOWELS = "aeiou";

const matcher = new RegExpMatcher({
  ...englishDataset.build(),
  ...englishRecommendedTransformers,
});

/**
 * The dataset's own words, harvested at runtime so no profanity is committed
 * here. Exported so a test can fail loudly if an obscenity upgrade changes the
 * metadata shape and this silently empties out.
 */
export const profanityWords: readonly string[] = (() => {
  const found: string[] = [];
  new DataSet<{ originalWord: string }>()
    .addAll(englishDataset)
    .removePhrasesIf((phrase) => {
      const word = phrase.metadata?.originalWord;
      // Short words ("ass", "cum") match inside too many real names to be
      // useful as substrings.
      if (word && word.length >= 4 && /^[a-z]+$/.test(word)) found.push(word);
      return false;
    });
  return found;
})();

/**
 * obscenity's patterns are word-bounded, but mangling a name can bury a word
 * inside a longer token where those patterns won't see it — so also check the
 * dataset's words as plain substrings.
 */
export function containsProfanity(token: string): boolean {
  const lower = token.toLowerCase();
  return (
    matcher.hasMatch(lower) || profanityWords.some((w) => lower.includes(w))
  );
}

type Rand = () => number;
type Strategy = (token: string, rand: Rand) => string;

// mulberry32 — small, seedable, and stable across runs so tests can pin output.
function rng(seed: number): Rand {
  let a = seed >>> 0;
  return () => {
    a = (a + 0x6d2b79f5) >>> 0;
    let t = Math.imul(a ^ (a >>> 15), 1 | a);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

function pick<T>(items: readonly T[], rand: Rand): T {
  return items[Math.floor(rand() * items.length)];
}

function indicesWhere(token: string, fn: (ch: string) => boolean): number[] {
  const out: number[] = [];
  for (let i = 0; i < token.length; i++) if (fn(token[i])) out.push(i);
  return out;
}

const vowelsIn = (t: string) => indicesWhere(t, (ch) => VOWELS.includes(ch));
const consonantsIn = (t: string) =>
  indicesWhere(t, (ch) => /[a-z]/.test(ch) && !VOWELS.includes(ch));

// --------------- Mechanical strategies ---------------

// Nearby-sounding vowels, not identical ones: Mark → Murk, Lisa → Leesa.
const DRIFT: Record<string, readonly string[]> = {
  a: ["u", "e"],
  e: ["i", "a"],
  i: ["ee", "ea"],
  o: ["aw", "ou"],
  u: ["o", "oo"],
};

const vowelDrift: Strategy = (t, rand) => {
  const idx = vowelsIn(t);
  if (!idx.length) return t;
  const i = pick(idx, rand);
  return t.slice(0, i) + pick(DRIFT[t[i]], rand) + t.slice(i + 1);
};

// Consonants that sound similar but aren't: Dave → Tave, Greg → Krek.
const SWAPS: Record<string, string> = {
  d: "t",
  t: "d",
  b: "p",
  p: "b",
  g: "k",
  k: "g",
  v: "b",
  n: "m",
  m: "n",
  c: "k",
};

const consonantConfusion: Strategy = (t, rand) => {
  const idx = indicesWhere(t, (ch) => ch in SWAPS);
  if (!idx.length) return t;
  const i = pick(idx, rand);
  return t.slice(0, i) + SWAPS[t[i]] + t.slice(i + 1);
};

// Double the wrong letter: Sarah → Sarrah, Jennifer → Jenniffer. Only
// consonants sitting between two vowels — doubling inside a cluster ("Stteve")
// reads as a typo rather than a mishearing.
const duplication: Strategy = (t, rand) => {
  const idx = consonantsIn(t).filter(
    (i) => i > 0 && VOWELS.includes(t[i - 1]) && VOWELS.includes(t[i + 1]),
  );
  if (!idx.length) return t;
  const i = pick(idx, rand);
  return t.slice(0, i + 1) + t[i] + t.slice(i + 1);
};

// Rewrite as it sounds, with wrong letter combos: Geoffrey → Jeffree.
const PHONETIC: readonly [RegExp, string][] = [
  [/ph/, "f"],
  [/th/, "t"],
  [/ch/, "k"],
  [/ck/, "k"],
  [/gh/, ""],
  [/qu/, "kw"],
  [/x/, "ks"],
  [/ie/, "ee"],
  [/y$/, "ee"],
  [/c(?=[eiy])/, "s"],
  [/c/, "k"],
];

const phoneticRespell: Strategy = (t) => {
  for (const [pattern, replacement] of PHONETIC) {
    const out = t.replace(pattern, replacement);
    if (out !== t) return out;
  }
  return t;
};

// The barista was sure they heard right: Priya → Preeya.
const confidentWrongness: Strategy = (t, rand) => {
  let out = t.replace(/i(?![aeiou])/, "ee");
  // Guard against vowel neighbours, or "Nguyen" becomes "Ngueeen".
  if (out === t) out = t.replace(/(?<![aeiou])y(?![aeiou])/, "ee");
  if (out === t) out = duplication(t, rand);
  return out;
};

// One surgical vowel change that shifts the whole vibe: Greg → Grog, Tina → Tuna.
const vibeSwap: Strategy = (t, rand) => {
  const idx = vowelsIn(t);
  if (!idx.length) return t;
  const i = pick(idx, rand);
  const others = VOWELS.split("").filter((v) => v !== t[i]);
  return t.slice(0, i) + pick(others, rand) + t.slice(i + 1);
};

// They started writing and just... stopped: Alexander → Alexan, Elizabeth →
// Elizab. Cut on a consonant — stopping mid-vowel ("Michae") reads as a
// truncation bug instead of someone giving up.
const trailOff: Strategy = (t) => {
  if (t.length < 6) return t;
  const start = Math.max(4, Math.ceil(t.length * 0.55));
  for (let keep = start; keep < t.length; keep++) {
    if (!VOWELS.includes(t[keep - 1])) return t.slice(0, keep);
  }
  return t.slice(0, start);
};

// --------------- Table strategies ---------------

// Heard something close and committed to it.
const MISHEARINGS: Record<string, string> = {
  nick: "neck",
  rick: "rock",
  matt: "mutt",
  brad: "bread",
  chloe: "cloey",
  megan: "meegan",
  kevin: "kelvin",
  dean: "bean",
  pete: "peat",
  jean: "gene",
};

// A real word that sounds like the name — per the old prompt, the funniest one.
const NOUNS: Record<string, string> = {
  karen: "carrot",
  bart: "bark",
  clara: "clam",
  phillip: "flipper",
  philip: "flipper",
  rick: "brick",
  brent: "brunt",
  grant: "grunt",
  greg: "grog",
  tina: "tuna",
  carl: "curl",
  mark: "mork",
  ross: "moss",
  glen: "glum",
  bill: "billy goat",
  hunter: "hunted",
};

// A different-gendered or alternate version of the name.
const VARIANTS: Record<string, string> = {
  daniel: "danielle",
  danielle: "daniel",
  chris: "christine",
  alex: "alexa",
  sam: "samantha",
  elizabeth: "elizabet",
  jessica: "jessico",
  michael: "michaela",
  nicole: "nichol",
  gabriel: "gabriella",
};

// Presumptuous about what the name is "really" short for.
const FULL_NAMES: Record<string, string> = {
  steve: "stefan",
  nat: "nathaniel",
  ben: "benedetto",
  meg: "meghan",
  dan: "dandrew",
  al: "alejandro",
  rob: "roberto",
  liz: "lizandria",
  pat: "patricius",
  mike: "mikael",
  tom: "tomassio",
  jen: "jennifera",
};

const fromTable =
  (table: Record<string, string>): Strategy =>
  (t) =>
    table[t] ?? t;

// --------------- Selection ---------------

// Seed buckets mirror the old prompt's guidance: low seeds stay subtle, high
// seeds go wild.
const SUBTLE: readonly Strategy[] = [vowelDrift, vibeSwap, duplication];
const MODERATE: readonly Strategy[] = [
  consonantConfusion,
  phoneticRespell,
  confidentWrongness,
  fromTable(VARIANTS),
];
const WILD: readonly Strategy[] = [
  fromTable(NOUNS),
  fromTable(FULL_NAMES),
  fromTable(MISHEARINGS),
  trailOff,
];

function shuffled<T>(items: readonly T[], rand: Rand): T[] {
  const out = [...items];
  for (let i = out.length - 1; i > 0; i--) {
    const j = Math.floor(rand() * (i + 1));
    [out[i], out[j]] = [out[j], out[i]];
  }
  return out;
}

function candidates(token: string, seed: number, rand: Rand): Strategy[] {
  const [first, ...rest] =
    seed <= 3
      ? [SUBTLE, MODERATE, WILD]
      : seed <= 6
        ? [MODERATE, WILD, SUBTLE]
        : [WILD, SUBTLE, MODERATE];
  const ordered = [
    ...shuffled(first, rand),
    ...rest.flatMap((bucket) => shuffled(bucket, rand)),
  ];
  // Short names have nothing to spare — add letters, don't remove them.
  return token.length <= 3
    ? ordered.filter((s) => s !== trailOff)
    : ordered;
}

// A name that is already profane is the customer's own; only reject a
// misspelling for *introducing* it.
function introducesProfanity(original: string, candidate: string): boolean {
  return containsProfanity(candidate) && !containsProfanity(original);
}

// Something has to change, even for a name every rule declines to touch.
function forceChange(token: string, rand: Rand): string {
  const idx = consonantsIn(token).filter((i) => i > 0);
  if (idx.length) {
    const i = pick(idx, rand);
    return token.slice(0, i + 1) + token[i] + token.slice(i + 1);
  }
  return token + pick(["y", "e", "o"], rand);
}

function matchCase(original: string, result: string): string {
  if (original === original.toUpperCase()) return result.toUpperCase();
  return result.charAt(0).toUpperCase() + result.slice(1);
}

/**
 * Misspell a customer's name the way a barista who half-heard it would.
 *
 * Only the first word is mangled; anything after it is preserved. Pass `seed`
 * to make the result reproducible — otherwise a random one varies the strategy
 * so repeat customers don't get the same misspelling every time.
 */
export function misspellName(name: string, seed?: number): string {
  const trimmed = name.trim().substring(0, 50);
  if (!trimmed) return name;

  const s = seed ?? Math.floor(Math.random() * 10);
  const rand = rng(s + trimmed.length * 31);

  const [head, ...tail] = trimmed.split(/\s+/);
  const token = head.toLowerCase();

  for (const strategy of candidates(token, s, rand)) {
    const out = strategy(token, rand);
    if (out === token || !out) continue;
    // Three of the same letter ("Weee") looks like a stutter, not a name.
    if (/(.)\1\1/.test(out) && !/(.)\1\1/.test(token)) continue;
    if (introducesProfanity(token, out)) continue;
    return [matchCase(head, out), ...tail].join(" ");
  }

  return [matchCase(head, forceChange(token, rand)), ...tail].join(" ");
}
