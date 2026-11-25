export type Theme = "light" | "dark"

/** The localStorage key the chosen theme is saved under. */
export const THEME_STORAGE_KEY = "theme"

// resolveTheme is the one rule for the theme: the saved one, dark when none is
// saved. It must stay self-contained (no imports, no outer names): its source
// is copied into the page by themeInitScript.
export function resolveTheme(stored: string | null): Theme {
  return !stored || stored === "dark" ? "dark" : "light"
}

// themeInitScript is the script vite.config.ts puts in the page's <head>. It
// sets the theme before the first paint, on every page, so no page shows the
// wrong theme while the app loads.
export function themeInitScript(): string {
  return (
    `document.documentElement.classList.toggle("dark",` +
    `(${resolveTheme.toString()})` +
    `(localStorage.getItem(${JSON.stringify(THEME_STORAGE_KEY)}))==="dark")`
  )
}
