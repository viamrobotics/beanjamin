/**
 * Barista Misspelling Test Harness
 *
 * Run with: npx tsx test-misspell.ts
 * Requires: ANTHROPIC_API_KEY env var set
 *
 * Tests the misspelling prompt against a batch of names and
 * prints results in a readable table for human review.
 */

import Anthropic from "@anthropic-ai/sdk";

const client = new Anthropic();

// Copy of the system prompt from the API route — keep in sync
const SYSTEM_PROMPT = `You are a barista name misspelling generator for a fun coffee shop app. Given a customer's name, return a plausibly wrong version — the kind of misspelling you'd see scrawled on a coffee cup by a barista who half-heard the name in a noisy café.

## MISSPELLING STRATEGIES (mix and match, surprise us)

**Vowel drift** — swap vowels for nearby-sounding ones, not identical ones:
- "a" ↔ "u" or "e", "i" ↔ "ee" ↔ "ea", "o" ↔ "aw" ↔ "ou"
- Mark → Murk, Lisa → Leesa, Tom → Tawm

**Consonant confusion** — swap consonants that sound *similar* but not identical:
- "d" ↔ "t", "b" ↔ "p", "g" ↔ "k", "v" ↔ "b", "n" ↔ "m"
- Dave → Tave, Greg → Krek

**Chaotic letter duplication or rearrangement** — add extra letters, double the wrong ones, scramble the middle:
- Michael → Micheaell, Jennifer → Jenniffer, Sarah → Sarrah
- Sometimes just slam an extra vowel in there: Brian → Briean

**Gender/variant swap** — switch to a different-gendered or alternate version of the name:
- Daniel → Danielle, Chris → Christine, Alex → Alexa, Sam → Samantha
- Or go the other way: Elizabeth → Elizabet, Jessica → Jessico

**Mishearing as a similar but WRONG name or word** — the barista heard something close but committed:
- Nick → Neck, Rick → Rock, Matt → Mutt, Brad → Bread
- Chloe → Cloey, Megan → Meegan, Kevin → Kelvin
- The result should sound *similar* but be clearly wrong or funny. NOT identical-sounding variants (don't do Sean → Shawn, that's boring).

**Phonetic respelling** — rewrite the name as it sounds using wrong letter combos:
- Geoffrey → Jeffree, Phoebe → Feebee, Yvonne → Eevon
- Catherine → Kathyrn, Stephen → Stefen

**Confident wrongness** — the barista was sure they heard right and wrote it with full conviction:
- "Alejandro" → "Allegandro", "Priya" → "Preeya", "Nguyen" → "Newin"

## RULES

1. Pick 1-3 strategies per name. Don't over-apply — one or two mutations is usually funnier than five.
2. The result should still be *vaguely recognizable* as the original name. If someone squints, they can tell what it was supposed to be.
3. Vary the intensity — sometimes it's a subtle single-letter swap, sometimes it's gloriously wrong.
4. SAFETY CHECK: Before returning, verify the misspelled name does NOT:
   - Form a slur, profanity, or offensive word in English (or look like one)
   - Resemble a derogatory term for any group
   - If it does, generate a different misspelling instead.
   Mildly silly or slightly rude results (like "Mutt" for "Matt") are fine — we want funny, not harmful.
5. For very short names (3 letters or fewer), lean toward mishearing or adding letters rather than removing them.
6. For names you don't recognize or unusual names, apply phonetic respelling or chaotic letter strategies.

## OUTPUT FORMAT

Respond with ONLY a JSON object, no markdown fencing:
{"misspelled": "the misspelled name", "strategy": "brief description of what you did"}`;

// ── Test names covering a range of origins, lengths, and difficulty ──

const TEST_NAMES = [
  // Common English names
  "Michael",
  "Jennifer",
  "Sarah",
  "Matt",
  "Nick",
  "Brad",
  "Rick",
  "Chloe",
  "Megan",
  "Kevin",
  "Mark",
  "Lisa",
  "Tom",
  "Dave",
  "Greg",
  "Daniel",
  "Chris",
  "Alex",
  "Sam",
  "Brian",
  // Names with tricky spellings
  "Catherine",
  "Geoffrey",
  "Phoebe",
  "Stephen",
  "Yvonne",
  // Short names
  "Ed",
  "Al",
  "Jo",
  "Ian",
  // International names
  "Alejandro",
  "Priya",
  "Nguyen",
  "Siobhan",
  "Dmitri",
  "Yuki",
  "Fatima",
  "Raj",
  "Aoife",
  "Björk",
];

interface MisspellResult {
  original: string;
  misspelled: string;
  strategy: string;
  error?: string;
}

async function misspellName(name: string): Promise<MisspellResult> {
  try {
    const message = await client.messages.create({
      model: "claude-sonnet-4-5-20250514",
      max_tokens: 150,
      system: SYSTEM_PROMPT,
      messages: [{ role: "user", content: `Customer name: "${name}"` }],
    });

    const text =
      message.content[0].type === "text" ? message.content[0].text : "";
    const result = JSON.parse(text);

    return {
      original: name,
      misspelled: result.misspelled,
      strategy: result.strategy,
    };
  } catch (error) {
    return {
      original: name,
      misspelled: "ERROR",
      strategy: "",
      error: String(error),
    };
  }
}

async function runTests() {
  console.log("☕ Barista Misspelling Test Harness");
  console.log("=".repeat(80));
  console.log(
    `Testing ${TEST_NAMES.length} names against the misspelling prompt...\n`
  );

  // Run in batches of 5 to avoid rate limits
  const results: MisspellResult[] = [];
  const BATCH_SIZE = 5;

  for (let i = 0; i < TEST_NAMES.length; i += BATCH_SIZE) {
    const batch = TEST_NAMES.slice(i, i + BATCH_SIZE);
    const batchResults = await Promise.all(batch.map(misspellName));
    results.push(...batchResults);

    // Progress indicator
    const done = Math.min(i + BATCH_SIZE, TEST_NAMES.length);
    process.stderr.write(`\r  Progress: ${done}/${TEST_NAMES.length}`);
  }

  console.log("\n");

  // Print results table
  const col1 = 15;
  const col2 = 20;
  const col3 = 40;

  console.log(
    "ORIGINAL".padEnd(col1) +
      "MISSPELLED".padEnd(col2) +
      "STRATEGY".padEnd(col3)
  );
  console.log("-".repeat(col1 + col2 + col3));

  for (const r of results) {
    if (r.error) {
      console.log(
        r.original.padEnd(col1) + "❌ ERROR".padEnd(col2) + r.error
      );
    } else {
      console.log(
        r.original.padEnd(col1) +
          r.misspelled.padEnd(col2) +
          r.strategy.substring(0, col3 - 2)
      );
    }
  }

  console.log("\n" + "=".repeat(80));

  // Quick stats
  const errors = results.filter((r) => r.error).length;
  const unchanged = results.filter(
    (r) => r.misspelled.toLowerCase() === r.original.toLowerCase()
  ).length;

  console.log(`\n📊 Results:`);
  console.log(`   Total: ${results.length}`);
  console.log(`   Errors: ${errors}`);
  console.log(
    `   Unchanged (prompt didn't misspell): ${unchanged} ${unchanged > 0 ? "⚠️" : "✅"}`
  );
  console.log(
    `\n👀 Review the table above. Look for:`
  );
  console.log(`   - Names that are TOO mangled (unrecognizable)`);
  console.log(`   - Names that are barely changed (boring)`);
  console.log(`   - Anything that reads as offensive`);
  console.log(`   - Short names that got shortened further (bad)`);
}

runTests().catch(console.error);