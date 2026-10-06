// Atkinson Hyperlegible Next and Mono (SIL Open Font License 1.1), from
// Fontsource. Designed for low-vision readers, they keep 0/O and 1/l/I distinct
// in ports, addresses, file names, and console output.
// Importing the files makes the bundler emit them as hashed assets; CSS url()
// references would be inlined into the main stylesheet instead. Only Latin and
// Latin Extended are registered; other scripts use the system fallbacks.
import sansLatin from "@fontsource-variable/atkinson-hyperlegible-next/files/atkinson-hyperlegible-next-latin-wght-normal.woff2";
import sansLatinExt from "@fontsource-variable/atkinson-hyperlegible-next/files/atkinson-hyperlegible-next-latin-ext-wght-normal.woff2";
import monoLatin from "@fontsource-variable/atkinson-hyperlegible-mono/files/atkinson-hyperlegible-mono-latin-wght-normal.woff2";
import monoLatinExt from "@fontsource-variable/atkinson-hyperlegible-mono/files/atkinson-hyperlegible-mono-latin-ext-wght-normal.woff2";

const LATIN = "U+0000-00FF, U+0131, U+0152-0153, U+02BB-02BC, U+02C6, U+02DA, U+02DC, U+0304, U+0308, U+0329, U+2000-206F, U+20AC, U+2122, U+2191, U+2193, U+2212, U+2215, U+FEFF, U+FFFD";
const LATIN_EXT = "U+0100-02BA, U+02BD-02C5, U+02C7-02CC, U+02CE-02D7, U+02DD-02FF, U+0304, U+0308, U+0329, U+1D00-1DBF, U+1E00-1E9F, U+1EF2-1EFF, U+2020, U+20A0-20AB, U+20AD-20C0, U+2113, U+2C60-2C7F, U+A720-A7FF";

const faces: [family: string, source: string, weight: string, unicodeRange: string][] = [
  ["Atkinson Hyperlegible Next", sansLatin, "200 800", LATIN],
  ["Atkinson Hyperlegible Next", sansLatinExt, "200 800", LATIN_EXT],
  ["Atkinson Hyperlegible Mono", monoLatin, "200 800", LATIN],
  ["Atkinson Hyperlegible Mono", monoLatinExt, "200 800", LATIN_EXT],
];

/** Registers the bundled faces; like @font-face, each file loads only when text needs it. */
export function registerFonts() {
  if (typeof FontFace === "undefined") return;
  for (const [family, source, weight, unicodeRange] of faces) {
    document.fonts.add(new FontFace(family, `url(${JSON.stringify(source)}) format("woff2")`, {
      weight,
      unicodeRange,
      display: "swap",
    }));
  }
}
