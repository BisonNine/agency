import { act, renderHook } from "@testing-library/react"
import { afterEach, beforeEach, describe, expect, it } from "vitest"

import { THEME_STORAGE_KEY } from "@/lib/theme"

import { useTheme } from "./use-theme"

describe("useTheme", () => {
  const realMatchMedia = window.matchMedia
  let systemDark = false
  let listeners: (() => void)[] = []

  // The system setting changes, as when the operating system switches theme.
  const setSystem = (dark: boolean) => {
    systemDark = dark
    act(() => listeners.forEach((l) => l()))
  }
  const isDark = () => document.documentElement.classList.contains("dark")

  beforeEach(() => {
    systemDark = false
    listeners = []
    window.matchMedia = (() => ({
      get matches() {
        return systemDark
      },
      addEventListener: (_type: string, l: () => void) => listeners.push(l),
      removeEventListener: (_type: string, l: () => void) => {
        listeners = listeners.filter((x) => x !== l)
      },
    })) as unknown as typeof window.matchMedia
  })

  afterEach(() => {
    window.matchMedia = realMatchMedia
    localStorage.clear()
    document.documentElement.classList.remove("dark")
  })

  it("follows the system, saving nothing, while no theme is chosen", () => {
    systemDark = true
    const { result } = renderHook(() => useTheme())
    expect(result.current.theme).toBe("dark")
    expect(isDark()).toBe(true)

    setSystem(false)
    expect(result.current.theme).toBe("light")
    expect(isDark()).toBe(false)

    setSystem(true)
    expect(result.current.theme).toBe("dark")
    expect(isDark()).toBe(true)
    expect(localStorage.getItem(THEME_STORAGE_KEY)).toBeNull()
  })

  it("saves the chosen theme and no longer follows the system", () => {
    const { result } = renderHook(() => useTheme())
    expect(result.current.theme).toBe("light")

    act(() => result.current.toggleTheme())
    expect(result.current.theme).toBe("dark")
    expect(isDark()).toBe(true)
    expect(localStorage.getItem(THEME_STORAGE_KEY)).toBe("dark")

    setSystem(true)
    setSystem(false)
    expect(result.current.theme).toBe("dark")
    expect(isDark()).toBe(true)

    act(() => result.current.toggleTheme())
    expect(result.current.theme).toBe("light")
    expect(localStorage.getItem(THEME_STORAGE_KEY)).toBe("light")
    setSystem(true)
    expect(result.current.theme).toBe("light")
  })

  it("does not follow the system when a theme was saved earlier", () => {
    localStorage.setItem(THEME_STORAGE_KEY, "light")
    const { result } = renderHook(() => useTheme())
    setSystem(true)
    expect(result.current.theme).toBe("light")
    expect(isDark()).toBe(false)
  })

  it("stops listening when it unmounts", () => {
    const { unmount } = renderHook(() => useTheme())
    expect(listeners).toHaveLength(1)
    unmount()
    expect(listeners).toHaveLength(0)
  })

  it("works when the system setting cannot be read", () => {
    // @ts-expect-error a browser without matchMedia
    window.matchMedia = undefined
    const { result } = renderHook(() => useTheme())
    expect(result.current.theme).toBe("light")
    act(() => result.current.toggleTheme())
    expect(result.current.theme).toBe("dark")
  })
})
