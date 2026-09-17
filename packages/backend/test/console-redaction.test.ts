import { describe, expect, it } from "bun:test";
import { ConsoleOutputRedactor } from "../src/console-redaction.js";

describe("streaming literal credential redaction", () => {
  const cases = [
    { secrets: [], input: "ordinary 🔑 log text", expected: "ordinary 🔑 log text" },
    { secrets: ["", "missing"], input: "ordinary log text", expected: "ordinary log text" },
    { secrets: ["secret-value", "secret"], input: "secret-value secret secret-value", expected: "[redacted] [redacted] [redacted]" },
    { secrets: ["secret", "secret-value"], input: "secret-value", expected: "[redacted]-value" },
    { secrets: ["aba", "bab"], input: "ababab", expected: "[redacted][redacted]" },
    { secrets: ["a.*[$]", "päss🔑word"], input: "before a.*[$] päss🔑word after", expected: "before [redacted] [redacted] after" },
    { secrets: ["s"], input: "sss", expected: "[redacted][redacted][redacted]" },
    { secrets: ["redacted", "token"], input: "token redacted", expected: "[redacted] [redacted]" },
    { secrets: ["secret"], input: "secre", expected: "secre" },
  ];
  for (const { secrets, input, expected } of cases) {
    it(`preserves literal matching across every split of ${JSON.stringify(input)}`, () => {
      const partitions = [input.split(""), ...Array.from({ length: input.length + 1 }, (_, split) => [input.slice(0, split), input.slice(split)])];
      for (const chunks of partitions) {
        let output = "";
        const redactor = new ConsoleOutputRedactor(secrets, value => { output += value; });
        for (const chunk of chunks) redactor.push(chunk);
        redactor.end();
        expect(output).toBe(expected);
      }
    });
  }

  it("withholds a possible credential suffix until the following chunk arrives", () => {
    let output = "";
    const redactor = new ConsoleOutputRedactor(["secret-value"], value => { output += value; });
    redactor.push("ordinary log text sec");
    expect(output).toBe("ordinary l");
    redactor.push("ret-value");
    expect(output).toBe("ordinary log text [redacted]");
    redactor.push(" more text following");
    expect(output).toContain("[redacted]");
    expect(output).not.toContain("secret-value");
    redactor.end();
    expect(output).toBe("ordinary log text [redacted] more text following");
  });

  it("redacts dense matches beside an absent credential and preserves a trailing match", () => {
    let output = "";
    const redactor = new ConsoleOutputRedactor(["absent-long-credential", "xy", "x"], value => { output += value; });
    redactor.push("xy".repeat(4096) + "x");
    redactor.push("y");
    redactor.end();
    expect(output).toBe("[redacted]".repeat(4097));
  });
});
