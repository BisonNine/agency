import { useCallback, useEffect, useState } from "react"

import { THEME_STORAGE_KEY, type Theme, resolveTheme } from "@/lib/theme"

function getStoredTheme(): Theme {
  if (typeof window === "undefined") return resolveTheme(null)
  return resolveTheme(localStorage.getItem(THEME_STORAGE_KEY))
}

export function useTheme() {
  const [theme, setThemeState] = useState<Theme>(getStoredTheme)

  useEffect(() => {
    const root = document.documentElement
    if (theme === "dark") {
      root.classList.add("dark")
    } else {
      root.classList.remove("dark")
    }
    localStorage.setItem(THEME_STORAGE_KEY, theme)
  }, [theme])

  const toggleTheme = useCallback(() => {
    setThemeState((prev) => (prev === "dark" ? "light" : "dark"))
  }, [])

  return { theme, toggleTheme }
}
