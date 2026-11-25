import { afterEach, beforeEach, describe, expect, it } from "vitest"

import {
  SYSTEM_DARK_QUERY,
  THEME_STORAGE_KEY,
  type Theme,
  currentTheme,
  themeInitScript,
} from "./theme"

// The rule is checked twice: as the app calls it, and as the script in the
// page's <head> applies it. The script runs as the browser would run it, from
// its text alone.
const subjects: [string, () => Theme][] = [
  ["currentTheme", currentTheme],
  [
    "themeInitScript",
    () => {
      new Function(themeInitScript())()
      return document.documentElement.classList.contains("dark")
        ? "dark"
        : "light"
    },
  ],
]

describe.each(subjects)("the theme rule (%s)", (_name, theme) => {
  const realMatchMedia = window.matchMedia

  const setSystem = (dark: boolean) => {
    window.matchMedia = ((query: string) => ({
      matches: query === SYSTEM_DARK_QUERY && dark,
    })) as typeof window.matchMedia
  }

  beforeEach(() => setSystem(false))

  afterEach(() => {
    window.matchMedia = realMatchMedia
    localStorage.clear()
    document.documentElement.classList.remove("dark")
  })

  it("is the saved light theme, whatever the system says", () => {
    setSystem(true)
    localStorage.setItem(THEME_STORAGE_KEY, "light")
    expect(theme()).toBe("light")
  })

  it("is the saved dark theme, whatever the system says", () => {
    setSystem(false)
    localStorage.setItem(THEME_STORAGE_KEY, "dark")
    expect(theme()).toBe("dark")
  })

  it("follows a dark system when nothing is saved", () => {
    setSystem(true)
    expect(theme()).toBe("dark")
  })

  it("follows a light system when nothing is saved", () => {
    document.documentElement.classList.add("dark")
    setSystem(false)
    expect(theme()).toBe("light")
  })

  it.each(["", "system", "DARK"])(
    "treats the saved value %j as nothing saved",
    (saved) => {
      localStorage.setItem(THEME_STORAGE_KEY, saved)
      setSystem(true)
      expect(theme()).toBe("dark")
      setSystem(false)
      expect(theme()).toBe("light")
    },
  )

  it("is light when the system setting cannot be read", () => {
    // @ts-expect-error a browser without matchMedia
    window.matchMedia = undefined
    expect(theme()).toBe("light")
  })

  it("is still the saved theme when the system setting cannot be read", () => {
    // @ts-expect-error a browser without matchMedia
    window.matchMedia = undefined
    localStorage.setItem(THEME_STORAGE_KEY, "dark")
    expect(theme()).toBe("dark")
  })

  it("follows the system when storage cannot be read", () => {
    const real = Object.getOwnPropertyDescriptor(window, "localStorage")!
    Object.defineProperty(window, "localStorage", {
      configurable: true,
      get() {
        throw new Error("storage is blocked")
      },
    })
    try {
      setSystem(true)
      expect(theme()).toBe("dark")
    } finally {
      Object.defineProperty(window, "localStorage", real)
    }
  })
})
