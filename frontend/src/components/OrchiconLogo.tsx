// The Orchicon brand mark — six segments of one ring, the last one carried in the brand signal green.
//
// WHY A COMPONENT RATHER THAN AN <img>: the mark has to follow the theme. Rendering it inline lets the
// five leading segments take `currentColor` (so they inherit the surrounding text colour and therefore read
// as ink in light themes and as light in dark ones), while the closing segment keeps the brand signal. An
// <img> of a fixed SVG could do neither without a second asset per theme, and the app ships 20+ themes.
//
// IT IS THE SAME TREATMENT THE WEBSITE USES (`.mark path{fill:currentColor}` +
// `.mark path.last{fill:var(--signal)}`), so the mark cannot drift between the two front doors.

/** The brand signal green — the one segment that is never `currentColor`. Matches the site's `--signal`. */
export const BRAND_SIGNAL = "#33CC99";

// The six arcs, in order. The last is the accent.
const SEGMENTS = [
  "M52.56 8.08A42 42 0 0 1 79.44 20.04L81.19 18.26L79.19 30.68L67.87 31.81L69.63 30.03A28 28 0 0 0 51.71 22.05Z",
  "M87.59 31.26A42 42 0 0 1 90.66 60.52L93.08 61.14L81.32 65.62L74.69 56.38L77.11 57.01A28 28 0 0 0 75.06 37.51Z",
  "M85.02 73.18A42 42 0 0 1 61.22 90.47L61.89 92.88L52.14 84.93L56.81 74.57L57.48 76.98A28 28 0 0 0 73.35 65.45Z",
  "M47.44 91.92A42 42 0 0 1 20.56 79.96L18.81 81.74L20.81 69.32L32.13 68.19L30.37 69.97A28 28 0 0 0 48.29 77.95Z",
  "M12.41 68.74A42 42 0 0 1 9.34 39.48L6.92 38.86L18.68 34.38L25.31 43.62L22.89 42.99A28 28 0 0 0 24.94 62.49Z",
  "M14.98 26.82A42 42 0 0 1 38.78 9.53L38.11 7.12L47.86 15.07L43.19 25.43L42.52 23.02A28 28 0 0 0 26.65 34.55Z",
];

type MarkProps = {
  /** Sizing and colour come from the caller — pass `text-foreground` and a width/height class. */
  className?: string;
  /**
   * Accessible name. OMIT IT when the mark sits beside the word "Orchicon", or the screen reader announces
   * the brand twice; pass it when the mark stands alone (the login page).
   */
  title?: string;
};

/**
 * The bare mark. Decorative by default, named on request — see `title`.
 */
export function OrchiconMark({ className, title }: MarkProps) {
  return (
    <svg
      viewBox="0 0 100 100"
      className={className}
      role={title ? "img" : undefined}
      aria-label={title}
      aria-hidden={title ? undefined : true}
    >
      {SEGMENTS.map((d, i) => (
        <path key={i} d={d} fill={i === SEGMENTS.length - 1 ? BRAND_SIGNAL : "currentColor"} />
      ))}
    </svg>
  );
}
