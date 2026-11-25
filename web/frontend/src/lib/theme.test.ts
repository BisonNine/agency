import { afterEach, describe, expect, it } from "vitest"

import { THEME_STORAGE_KEY, resolveTheme, themeInitScript } from "./theme"

describe("resolveTheme", () => {
  it("is dark when nothing is saved", () => {
    expect(resolveTheme(null)).toBe("dark")
    expect(resolveTheme("")).toBe("dark")
  })

  it("is the saved theme", () => {
    expect(resolveTheme("dark")).toBe("dark")
    expect(resolveTheme("light")).toBe("light")
  })
})

// The script runs in the page before the app loads; here it runs as the
// browser would run it, from its text alone.
describe("themeInitScript", () => {
  const run = () => new Function(themeInitScript())()
  const isDark = () => document.documentElement.classList.contains("dark")

  afterEach(() => {
    localStorage.clear()
    document.documentElement.classList.remove("dark")
  })

  it("sets the dark theme when nothing is saved", () => {
    run()
    expect(isDark()).toBe(true)
  })

  it("sets the saved dark theme", () => {
    localStorage.setItem(THEME_STORAGE_KEY, "dark")
    run()
    expect(isDark()).toBe(true)
  })

  it("sets the saved light theme", () => {
    document.documentElement.classList.add("dark")
    localStorage.setItem(THEME_STORAGE_KEY, "light")
    run()
    expect(isDark()).toBe(false)
  })
})
