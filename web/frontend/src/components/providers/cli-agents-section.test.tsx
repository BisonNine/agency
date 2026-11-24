import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { fireEvent, render, screen, waitFor } from "@testing-library/react"
import { beforeEach, describe, expect, it, vi } from "vitest"

import type { CLIInfo, CLIRegistration, CLIRegistrations } from "@/api/system"

import { CLIAgentsSection } from "./cli-agents-section"

vi.mock("react-i18next", () => ({
  useTranslation: () => ({
    t: (key: string, opts?: Record<string, unknown>) =>
      opts ? `${key}:${JSON.stringify(opts)}` : key,
  }),
}))

vi.mock("@tanstack/react-router", () => ({
  Link: ({
    children,
    to,
    ...rest
  }: {
    children: React.ReactNode
    to: string
  } & Record<string, unknown>) => (
    <a href={to} {...rest}>
      {children}
    </a>
  ),
}))

const setCLIEnabled = vi.fn()
const setCLIBypassRestrictions = vi.fn()
const listCLIs = vi.fn()
const getCLIRegistrations = vi.fn()
const registerCLI = vi.fn()
vi.mock("@/api/system", async () => ({
  listCLIs: (...a: unknown[]) => listCLIs(...a),
  setCLIEnabled: (...a: unknown[]) => setCLIEnabled(...a),
  setCLIBypassRestrictions: (...a: unknown[]) => setCLIBypassRestrictions(...a),
  getCLIRegistrations: (...a: unknown[]) => getCLIRegistrations(...a),
  registerCLI: (...a: unknown[]) => registerCLI(...a),
}))

function cli(over: Partial<CLIInfo> = {}): CLIInfo {
  return {
    protocol: "antigravity-cli",
    label: "Antigravity CLI",
    binary: "agy",
    installed: true,
    enabled: false,
    configured: false,
    provider_index: -1,
    models: 0,
    models_enabled: 0,
    base_args: ["--output-format", "json"],
    required_args: [],
    bypass_args: ["--dangerously-skip-permissions"],
    bypass_restrictions: false,
    ...over,
  }
}

const URL_HERE = "http://127.0.0.1:5911/internal"

function registrations(
  clis: CLIRegistration[],
  url = URL_HERE,
): CLIRegistrations {
  return { url, clis }
}

function renderSection(
  rows: CLIInfo[],
  regs: CLIRegistrations = registrations([]),
) {
  getCLIRegistrations.mockResolvedValue(regs)
  listCLIs.mockResolvedValue(rows)
  setCLIEnabled.mockResolvedValue(undefined)
  setCLIBypassRestrictions.mockResolvedValue(undefined)
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={qc}>
      <CLIAgentsSection providers={[]} onEdit={() => {}} />
    </QueryClientProvider>,
  )
}

describe("CLIAgentsSection", () => {
  it("switches a CLI on with one click and no fields", async () => {
    renderSection([cli()])
    fireEvent.click(await screen.findByTestId("cli-switch-antigravity-cli"))
    await waitFor(() =>
      expect(setCLIEnabled).toHaveBeenCalledWith("antigravity-cli", true),
    )
  })

  it("lists a CLI whose binary is missing, greyed and unswitchable", async () => {
    // Hiding it would look like the product does not support the CLI, when the truth
    // is that the host does not have it.
    renderSection([cli({ installed: false })])
    const row = await screen.findByTestId("cli-row-antigravity-cli")
    expect(row.className).toContain("opacity-55")
    expect(
      screen.getByTestId("cli-switch-antigravity-cli").hasAttribute("disabled"),
    ).toBe(true)
    expect(screen.getByText(/notFound/)).toBeTruthy()
  })

  it("shows the resolved path once installed", async () => {
    renderSection([cli({ path: "/home/admin/.local/bin/agy" })])
    expect(await screen.findByText("/home/admin/.local/bin/agy")).toBeTruthy()
  })

  it("says how many models a switch governs when it is more than one", async () => {
    // Turning the CLI off disables all of them, so the count must be visible
    // before the switch is flipped, not after.
    renderSection([
      cli({
        protocol: "claude-cli",
        configured: true,
        models: 3,
        models_enabled: 2,
        enabled: true,
      }),
    ])
    expect(await screen.findByText(/modelCount/)).toBeTruthy()
  })

  it("shows the count on a single-model row too", async () => {
    // Printing it for a CLI with three models and omitting it for the rest
    // reads as a fault rather than as brevity.
    renderSection([
      cli({ configured: true, models: 1, models_enabled: 1, enabled: true }),
    ])
    expect(await screen.findByText(/modelCount/)).toBeTruthy()
  })

  it("shows no count for a CLI that is not configured", async () => {
    // Nothing to count, and "0 of 0" is noise on a row whose point is that it
    // has not been set up.
    renderSection([cli({ configured: false, models: 0 })])
    await screen.findByTestId("cli-row-antigravity-cli")
    expect(screen.queryByText(/modelCount/)).toBeNull()
  })

  it("shows the flags it will run with, so nobody has to guess", async () => {
    // Someone deciding whether to switch a CLI on is entitled to read them
    // here rather than find them in a process listing.
    renderSection([
      cli({
        base_args: ["-p", "--output-format", "json"],
        required_args: ["--no-chrome"],
        bypass_args: ["--yolo"],
        extra_args: ["--verbose"],
        trailing_args: ["-"],
      }),
    ])
    const row = await screen.findByTestId("cli-row-antigravity-cli")
    // The whole command line, in invocation order — the provider's own flags
    // included, not just the ones that live in config. The bypass flag is
    // absent while the checkbox is off, because it is not passed.
    expect(row.textContent).toContain(
      "-p --output-format json --no-chrome --verbose -",
    )
    expect(row.textContent).not.toContain("--yolo")
  })

  it("shows the bypass flag in the command line only when the setting is on", async () => {
    renderSection([
      cli({
        base_args: ["-p"],
        bypass_args: ["--yolo"],
        bypass_restrictions: true,
        configured: true,
      }),
    ])
    const row = await screen.findByTestId("cli-row-antigravity-cli")
    expect(row.textContent).toContain("-p --yolo")
  })

  it("offers the bypass checkbox, unticked, once a provider exists", async () => {
    // The setting lives on the provider, so there is nowhere to write it until
    // the switch has created one.
    const { unmount } = renderSection([cli()])
    await screen.findByTestId("cli-row-antigravity-cli")
    expect(screen.queryByTestId("cli-bypass-antigravity-cli")).toBeNull()
    unmount()

    renderSection([cli({ configured: true, provider_index: 0 })])
    const box = await screen.findByTestId("cli-bypass-antigravity-cli")
    expect(box.getAttribute("data-state")).toBe("unchecked")
    expect(screen.getByText("providers.cli.bypassHint")).toBeTruthy()
    fireEvent.click(box)
    await waitFor(() =>
      expect(setCLIBypassRestrictions).toHaveBeenCalledWith(
        "antigravity-cli",
        true,
      ),
    )
    // Ticking it never touches the enable switch.
    expect(setCLIEnabled).not.toHaveBeenCalled()
  })

  it("offers editing only once a provider exists", async () => {
    const { unmount } = renderSection([cli()])
    await screen.findByTestId("cli-row-antigravity-cli")
    expect(screen.queryByTestId("cli-edit-antigravity-cli")).toBeNull()
    unmount()

    renderSection([cli({ configured: true, provider_index: 3 })])
    expect(await screen.findByTestId("cli-edit-antigravity-cli")).toBeTruthy()
  })

  it("surfaces a failure rather than silently leaving the switch unchanged", async () => {
    listCLIs.mockResolvedValue([cli()])
    setCLIEnabled.mockRejectedValue(new Error("config is read-only"))
    const qc = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    })
    render(
      <QueryClientProvider client={qc}>
        <CLIAgentsSection providers={[]} onEdit={() => {}} />
      </QueryClientProvider>,
    )
    fireEvent.click(await screen.findByTestId("cli-switch-antigravity-cli"))
    expect(await screen.findByText("config is read-only")).toBeTruthy()
  })

  describe("tool server registration", () => {
    beforeEach(() => {
      registerCLI.mockReset()
      getCLIRegistrations.mockReset()
    })

    const agy = (over: Partial<CLIRegistration> = {}): CLIRegistration => ({
      protocol: "antigravity-cli",
      label: "Antigravity CLI",
      state: "not_registered",
      ...over,
    })

    it.each([
      ["registered", false],
      ["not_registered", false],
      ["other_address", false],
      ["not_checked", false],
      ["not_installed", true],
      ["host_off", true],
    ] as const)("labels the %s state", async (state, disabled) => {
      renderSection([cli()], registrations([agy({ state })]))
      const label = await screen.findByTestId(
        "cli-register-state-antigravity-cli",
      )
      await waitFor(() => expect(label.getAttribute("data-state")).toBe(state))
      expect(label.textContent).toBe(`providers.cli.register.state.${state}`)
      const button = screen.getByTestId("cli-register-button-antigravity-cli")
      expect(button.hasAttribute("disabled")).toBe(disabled)
      expect(button.textContent).toBe(
        state === "other_address"
          ? "providers.cli.register.replace"
          : "providers.cli.register.button",
      )
    })

    it("links the host-off label to the setting that turns it on", async () => {
      renderSection([cli()], registrations([agy({ state: "host_off" })], ""))
      const link = await screen.findByTestId(
        "cli-register-host-off-antigravity-cli",
      )
      expect(link.getAttribute("href")).toBe("/mcp/config")
    })

    it("shows the other address beside the label", async () => {
      renderSection(
        [cli()],
        registrations([
          agy({ state: "other_address", address: "http://127.0.0.1:7000/x" }),
        ]),
      )
      expect(
        (await screen.findByTestId("cli-register-address-antigravity-cli"))
          .textContent,
      ).toBe("http://127.0.0.1:7000/x")
    })

    it("is disabled while the states are read", async () => {
      getCLIRegistrations.mockReturnValue(new Promise(() => {}))
      listCLIs.mockResolvedValue([cli()])
      const qc = new QueryClient({
        defaultOptions: { queries: { retry: false } },
      })
      render(
        <QueryClientProvider client={qc}>
          <CLIAgentsSection providers={[]} onEdit={() => {}} />
        </QueryClientProvider>,
      )
      const label = await screen.findByTestId(
        "cli-register-state-antigravity-cli",
      )
      expect(label.getAttribute("data-state")).toBe("checking")
      expect(
        screen
          .getByTestId("cli-register-button-antigravity-cli")
          .hasAttribute("disabled"),
      ).toBe(true)
    })

    it("registers and updates the row without a reload", async () => {
      registerCLI.mockResolvedValue(agy({ state: "registered" }))
      renderSection([cli()], registrations([agy()]))
      const button = await screen.findByTestId(
        "cli-register-button-antigravity-cli",
      )
      await waitFor(() => expect(button.hasAttribute("disabled")).toBe(false))
      fireEvent.click(button)
      await waitFor(() =>
        expect(registerCLI).toHaveBeenCalledWith("antigravity-cli", false),
      )
      expect(
        (await screen.findByTestId("cli-register-notice")).textContent,
      ).toBe('providers.cli.register.done:{"label":"Antigravity CLI"}')
      await waitFor(() =>
        expect(
          screen
            .getByTestId("cli-register-state-antigravity-cli")
            .getAttribute("data-state"),
        ).toBe("registered"),
      )
      expect(getCLIRegistrations).toHaveBeenCalledTimes(1)
    })

    it("asks before replacing, naming both addresses", async () => {
      registerCLI.mockResolvedValue(agy({ state: "registered" }))
      renderSection(
        [cli()],
        registrations([
          agy({ state: "other_address", address: "http://127.0.0.1:7000/x" }),
        ]),
      )
      const button = await screen.findByTestId(
        "cli-register-button-antigravity-cli",
      )
      await waitFor(() => expect(button.textContent).toContain("replace"))
      fireEvent.click(button)
      const dialog = await screen.findByTestId("cli-register-confirm")
      expect(dialog.textContent).toContain(
        `providers.cli.register.confirmBody:${JSON.stringify({
          label: "Antigravity CLI",
          old: "http://127.0.0.1:7000/x",
          new: URL_HERE,
        })}`,
      )
      expect(registerCLI).not.toHaveBeenCalled()
      fireEvent.click(screen.getByTestId("cli-register-confirm-replace"))
      await waitFor(() =>
        expect(registerCLI).toHaveBeenCalledWith("antigravity-cli", true),
      )
    })

    it("replaces nothing when the confirmation is cancelled", async () => {
      renderSection(
        [cli()],
        registrations([agy({ state: "other_address", address: "http://x/y" })]),
      )
      const button = await screen.findByTestId(
        "cli-register-button-antigravity-cli",
      )
      await waitFor(() => expect(button.hasAttribute("disabled")).toBe(false))
      fireEvent.click(button)
      fireEvent.click(await screen.findByText("common.cancel"))
      await waitFor(() =>
        expect(screen.queryByTestId("cli-register-confirm")).toBeNull(),
      )
      expect(registerCLI).not.toHaveBeenCalled()
    })

    it("shows the server's sentence when registering fails, and reads again", async () => {
      registerCLI.mockRejectedValue(
        new Error("Could not register Antigravity CLI."),
      )
      renderSection([cli()], registrations([agy()]))
      const button = await screen.findByTestId(
        "cli-register-button-antigravity-cli",
      )
      await waitFor(() => expect(button.hasAttribute("disabled")).toBe(false))
      fireEvent.click(button)
      expect(
        await screen.findByText("Could not register Antigravity CLI."),
      ).toBeTruthy()
      await waitFor(() => expect(getCLIRegistrations).toHaveBeenCalledTimes(2))
    })

    it("names the CLI when the failure carries no sentence", async () => {
      registerCLI.mockRejectedValue("nope")
      renderSection([cli()], registrations([agy()]))
      const button = await screen.findByTestId(
        "cli-register-button-antigravity-cli",
      )
      await waitFor(() => expect(button.hasAttribute("disabled")).toBe(false))
      fireEvent.click(button)
      expect(
        await screen.findByText(
          'providers.cli.register.failed:{"label":"Antigravity CLI"}',
        ),
      ).toBeTruthy()
    })
  })
})
