interface LogoProps {
  /** "full" is the mark and the word; "mark" is the mark alone. */
  variant: "full" | "mark"
  className?: string
  /** The product name where the logo is the only name shown; empty otherwise. */
  alt: string
}

// Logo draws the logo in the version for the current theme. Each variant has
// two files, switched in CSS by the same `dark` class that themes the page, so
// the logo always matches the background it is drawn on.
export function Logo({ variant, className = "", alt }: LogoProps) {
  const file = variant === "full" ? "logo" : "mark"
  return (
    <>
      <img
        className={`w-auto dark:hidden ${className}`}
        src={`/${file}-light.png`}
        alt={alt}
      />
      <img
        className={`hidden w-auto dark:block ${className}`}
        src={`/${file}-dark.png`}
        alt={alt}
      />
    </>
  )
}
