import { render, screen } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import { EMPTY_MCP_FORM } from "./form-model"
import { EnableSection } from "./mcp-sections"

vi.mock("react-i18next", () => ({
  useTranslation: () => ({ t: (key: string) => key }),
}))

vi.mock("@tanstack/react-router", () => ({
  Link: ({
    children,
    to,
    hash,
  }: {
    children: React.ReactNode
    to: string
    hash?: string
  }) => <a href={hash ? `${to}#${hash}` : to}>{children}</a>,
}))

describe("EnableSection", () => {
  // The CLI providers' Register tool server button depends on the tool
  // server: while it is off, this page says so and links back.
  it("says CLIs can't be registered while the tool server is off", () => {
    render(
      <EnableSection
        form={{ ...EMPTY_MCP_FORM, enabled: false, autoEnable: false }}
        onFieldChange={() => {}}
      />,
    )
    const note = screen.getByTestId("mcp-host-off-note")
    expect(note.textContent).toContain("pages.mcp.off_note")
    expect(
      screen.getByText("pages.mcp.off_note_link").getAttribute("href"),
    ).toBe("/providers#cli-agents")
  })

  it.each([
    [true, false],
    [false, true],
  ])("has no note when enabled=%s, auto=%s", (enabled, autoEnable) => {
    render(
      <EnableSection
        form={{ ...EMPTY_MCP_FORM, enabled, autoEnable }}
        onFieldChange={() => {}}
      />,
    )
    expect(screen.queryByTestId("mcp-host-off-note")).toBeNull()
  })
})
