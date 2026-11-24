import { describe, expect, it } from "vitest"

import { SETUP_DISMISSED_KEY } from "./dismissed"

describe("setup dismissed key", () => {
  it("is named after the product", () => {
    expect(SETUP_DISMISSED_KEY).toBe("agency.setupDismissed")
  })
})
