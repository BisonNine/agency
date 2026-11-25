export type Theme = "light" | "dark"

/** The localStorage key the chosen theme is saved under. */
export const THEME_STORAGE_KEY = "agency:theme"

/** The media query that says the system is set to dark. */
export const SYSTEM_DARK_QUERY = "(prefers-color-scheme: dark)"

// resolveTheme is the one rule for the theme: the one the user chose with the
// theme button, else the system's. A saved value that is not a theme counts as
// nothing saved. Where the system setting cannot be read (no matchMedia) the
// theme is light: the rule is dark only when the system says dark, and light
// is what a browser itself reports when it has no preference.
//
// It must stay self-contained (no imports, no outer names) and must not throw:
// its source is copied into the page by themeInitScript and runs before
// anything else, where storage may be blocked and matchMedia may be missing.
export function resolveTheme(
  storageKey: string,
  systemDarkQuery: string,
): Theme {
  try {
    const saved = localStorage.getItem(storageKey)
    if (saved === "light" || saved === "dark") return saved
  } catch {
    // Storage is blocked: nothing is saved.
  }
  try {
    // Typed here because this file is also compiled for vite.config.ts,
    // without the browser's types.
    const page = globalThis as unknown as {
      matchMedia(query: string): { matches: boolean }
    }
    return page.matchMedia(systemDarkQuery).matches ? "dark" : "light"
  } catch {
    return "light"
  }
}

/** The theme to show now. */
export function currentTheme(): Theme {
  return resolveTheme(THEME_STORAGE_KEY, SYSTEM_DARK_QUERY)
}

// themeInitScript is the script vite.config.ts puts in the page's <head>. It
// sets the theme before the first paint, on every page, so no page shows the
// wrong theme while the app loads.
export function themeInitScript(): string {
  return (
    `document.documentElement.classList.toggle("dark",` +
    `(${resolveTheme.toString()})` +
    `(${JSON.stringify(THEME_STORAGE_KEY)},${JSON.stringify(SYSTEM_DARK_QUERY)})==="dark")`
  )
}
