interface LogoProps {
  className?: string
  /** The product name where the logo is the only name shown; empty otherwise. */
  alt: string
}

// Logo is the full logo (mark and word). Two files, one per theme, switched in
// CSS by the same `dark` class that themes the page, so the logo always
// matches the background it is drawn on.
export function Logo({ className = "", alt }: LogoProps) {
  return (
    <>
      <img
        className={`w-auto dark:hidden ${className}`}
        src="/logo-light.png"
        alt={alt}
      />
      <img
        className={`hidden w-auto dark:block ${className}`}
        src="/logo-dark.png"
        alt={alt}
      />
    </>
  )
}
