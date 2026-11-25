import { useCallback, useEffect, useState } from "react"

import {
  SYSTEM_DARK_QUERY,
  THEME_STORAGE_KEY,
  type Theme,
  currentTheme,
} from "@/lib/theme"

export function useTheme() {
  const [theme, setTheme] = useState<Theme>(currentTheme)

  useEffect(() => {
    document.documentElement.classList.toggle("dark", theme === "dark")
  }, [theme])

  // Follow the system while it changes. currentTheme puts a saved choice
  // first, so once the user has chosen this changes nothing.
  useEffect(() => {
    if (typeof window.matchMedia !== "function") return
    const system = window.matchMedia(SYSTEM_DARK_QUERY)
    const onChange = () => setTheme(currentTheme())
    system.addEventListener("change", onChange)
    return () => system.removeEventListener("change", onChange)
  }, [])

  // The theme is saved only here, when the user chooses one.
  const toggleTheme = useCallback(() => {
    const next: Theme = theme === "dark" ? "light" : "dark"
    try {
      localStorage.setItem(THEME_STORAGE_KEY, next)
    } catch {
      // Storage is blocked: the choice lasts until the page is reloaded.
    }
    setTheme(next)
  }, [theme])

  return { theme, toggleTheme }
}
