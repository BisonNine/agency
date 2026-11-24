import { describe, expect, it } from "vitest"

import fixture from "../../../../../config/testdata/provider_protocols.json"
import {
  DEFAULT_PROTOCOL,
  PROTOCOL_OPTIONS,
  isCliProtocol,
} from "./provider-config-fields"

// config/provider_protocols_test.go checks the same file against the
// protocols the backend accepts, so the forms never offer or start with one
// a save would refuse.
describe("provider protocols", () => {
  it("defaults the provider forms to the server's default protocol", () => {
    expect(DEFAULT_PROTOCOL).toBe(fixture.default_protocol)
    expect(fixture.protocols).toContain(DEFAULT_PROTOCOL)
  })

  it("offers only protocols the server accepts", () => {
    for (const p of PROTOCOL_OPTIONS) {
      expect(fixture.protocols).toContain(p)
    }
  })

  it("renders every CLI protocol the server accepts as a CLI", () => {
    const cli = fixture.protocols.filter((p) => p.endsWith("-cli"))
    expect(cli).toContain("opencode-cli")
    for (const p of cli) {
      expect(isCliProtocol(p)).toBe(true)
    }
  })
})
