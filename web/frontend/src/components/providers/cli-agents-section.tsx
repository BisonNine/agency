import { IconEdit, IconLoader2 } from "@tabler/icons-react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { useState } from "react"
import { useTranslation } from "react-i18next"

import type { ProviderInfo } from "@/api/providers"
import {
  type CLIInfo,
  type CLIRegistration,
  type CLIRegistrations,
  getCLIRegistrations,
  listCLIs,
  registerCLI,
  setCLIBypassRestrictions,
  setCLIEnabled,
} from "@/api/system"
import {
  CLIRegisterLine,
  ReplaceRegistrationDialog,
} from "@/components/providers/cli-register"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Switch } from "@/components/ui/switch"

/**
 * The CLI agents section.
 *
 * A CLI agent is one thing to the person using it and three to the
 * configuration: a provider, a model, and flags that are mandatory and
 * invisible. Building that by hand meant two pages and four values that appear
 * in no form, and getting them wrong failed silently. Here it is one switch,
 * and the backend creates whatever is missing.
 *
 * Every supported CLI is listed whether or not its binary is present. A CLI
 * Agency supports but the host lacks is a different thing from one it does not
 * support, and hiding the row makes it look like the latter — so a missing one
 * is greyed out and says so.
 */
export function CLIAgentsSection({
  providers,
  onEdit,
}: {
  providers: ProviderInfo[]
  onEdit: (provider: ProviderInfo) => void
}) {
  const { t } = useTranslation()
  const queryClient = useQueryClient()
  const [error, setError] = useState("")
  const [notice, setNotice] = useState("")
  const [confirmReplace, setConfirmReplace] = useState<CLIRegistration | null>(
    null,
  )

  const { data: clis = [], isPending } = useQuery({
    queryKey: ["system-clis"],
    queryFn: listCLIs,
  })

  // Read separately: it asks each CLI, so the rows render first and their
  // labels fill in.
  const { data: registrations } = useQuery({
    queryKey: ["cli-registration"],
    queryFn: getCLIRegistrations,
  })

  const register = useMutation({
    mutationFn: ({
      target,
      replace,
    }: {
      target: CLIRegistration
      replace: boolean
    }) => registerCLI(target.protocol, replace),
    onMutate: () => {
      setError("")
      setNotice("")
    },
    onSuccess: (st) => {
      queryClient.setQueryData<CLIRegistrations>(["cli-registration"], (old) =>
        old
          ? {
              ...old,
              clis: old.clis.map((c) => (c.protocol === st.protocol ? st : c)),
            }
          : old,
      )
      void queryClient.invalidateQueries({ queryKey: ["report-assessment"] })
      if (st.state === "registered") {
        setNotice(t("providers.cli.register.done", { label: st.label }))
      }
    },
    onError: (e: unknown, { target }) => {
      setError(
        e instanceof Error && e.message
          ? e.message
          : t("providers.cli.register.failed", { label: target.label }),
      )
      // The server read the CLI again; show what it found.
      void queryClient.invalidateQueries({ queryKey: ["cli-registration"] })
    },
  })

  const startRegister = (target: CLIRegistration) => {
    if (target.state === "other_address") {
      setConfirmReplace(target)
      return
    }
    register.mutate({ target, replace: false })
  }

  const toggle = useMutation({
    mutationFn: ({
      protocol,
      enabled,
    }: {
      protocol: string
      enabled: boolean
    }) => setCLIEnabled(protocol, enabled),
    onMutate: () => setError(""),
    onSuccess: () => {
      // Turning a CLI on creates a provider and a model, so both lists are
      // stale, not just this one.
      void queryClient.invalidateQueries({ queryKey: ["system-clis"] })
      void queryClient.invalidateQueries({ queryKey: ["providers"] })
      void queryClient.invalidateQueries({ queryKey: ["models"] })
      // A CLI switched on can start the tool server (auto-enable).
      void queryClient.invalidateQueries({ queryKey: ["cli-registration"] })
    },
    onError: (e: unknown) =>
      setError(e instanceof Error ? e.message : t("providers.cli.toggleError")),
  })

  const bypass = useMutation({
    mutationFn: ({ protocol, on }: { protocol: string; on: boolean }) =>
      setCLIBypassRestrictions(protocol, on),
    onMutate: () => setError(""),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["system-clis"] })
      void queryClient.invalidateQueries({ queryKey: ["providers"] })
    },
    onError: (e: unknown) =>
      setError(e instanceof Error ? e.message : t("providers.cli.toggleError")),
  })

  if (isPending) {
    return (
      <div className="flex items-center gap-2 py-6">
        <IconLoader2 className="text-muted-foreground size-4 animate-spin" />
      </div>
    )
  }

  return (
    <section className="pt-6" id="cli-agents" data-testid="cli-agents">
      <h2 className="text-foreground text-sm font-semibold">
        {t("providers.cli.title")}
      </h2>
      <p className="text-muted-foreground mt-1 text-sm">
        {t("providers.cli.description")}
      </p>

      {error && (
        <p className="text-destructive bg-destructive/10 mt-3 rounded-md px-3 py-2 text-sm">
          {error}
        </p>
      )}
      {notice && (
        <p
          className="mt-3 rounded-md bg-green-500/10 px-3 py-2 text-sm text-green-700 dark:text-green-400"
          data-testid="cli-register-notice"
        >
          {notice}
        </p>
      )}

      <div className="border-border divide-border mt-3 divide-y rounded-lg border">
        {clis.map((cli) => (
          <CLIRow
            key={cli.protocol}
            cli={cli}
            busy={
              toggle.isPending && toggle.variables?.protocol === cli.protocol
            }
            registration={registrations?.clis.find(
              (r) => r.protocol === cli.protocol,
            )}
            registering={
              register.isPending &&
              register.variables?.target.protocol === cli.protocol
            }
            onRegister={startRegister}
            onToggle={(enabled) =>
              toggle.mutate({ protocol: cli.protocol, enabled })
            }
            onBypass={(on) => bypass.mutate({ protocol: cli.protocol, on })}
            onEdit={() => {
              // The switch covers everything most people need. Editing is for
              // the rest — chiefly pinning an explicit binary path — and reuses
              // the ordinary provider sheet rather than a second form.
              const p = providers.find((x) => x.index === cli.provider_index)
              if (p) onEdit(p)
            }}
          />
        ))}
      </div>

      <ReplaceRegistrationDialog
        target={confirmReplace}
        url={registrations?.url ?? ""}
        onCancel={() => setConfirmReplace(null)}
        onConfirm={(target) => {
          setConfirmReplace(null)
          register.mutate({ target, replace: true })
        }}
      />
    </section>
  )
}

function CLIRow({
  cli,
  busy,
  registration,
  registering,
  onRegister,
  onToggle,
  onBypass,
  onEdit,
}: {
  cli: CLIInfo
  busy: boolean
  registration?: CLIRegistration
  registering: boolean
  onRegister: (target: CLIRegistration) => void
  onToggle: (enabled: boolean) => void
  onBypass: (on: boolean) => void
  onEdit: () => void
}) {
  const { t } = useTranslation()
  // The whole command line, in the order it is actually built: the provider's
  // own flags, the bypass flag only when the checkbox is ticked, the headless
  // flags, whatever the models add, then the stdin marker. Showing only the
  // configured part would answer "what runs on my machine" with the smaller
  // half of the truth.
  const args = [
    ...cli.base_args,
    ...(cli.bypass_restrictions ? cli.bypass_args : []),
    ...cli.required_args,
    ...(cli.extra_args ?? []),
    ...(cli.trailing_args ?? []),
  ]

  return (
    <div
      data-testid={`cli-row-${cli.protocol}`}
      className={[
        "flex items-center gap-3 px-4 py-3",
        cli.installed ? "" : "opacity-55",
      ].join(" ")}
    >
      <span
        className={[
          "size-2 shrink-0 rounded-full",
          cli.enabled
            ? "bg-green-500"
            : cli.installed
              ? "bg-muted-foreground/40"
              : "bg-muted-foreground/20",
        ].join(" ")}
      />

      <div className="min-w-0 flex-1">
        <div className="text-foreground text-sm font-medium">{cli.label}</div>
        <div className="text-muted-foreground truncate font-mono text-xs">
          {cli.installed
            ? (cli.path ?? cli.binary)
            : t("providers.cli.notFound", { binary: cli.binary })}
        </div>
        {/* Some of these auto-approve tool use. Someone deciding whether to run
            a CLI unattended is entitled to read the command line here rather
            than find it in a process listing. */}
        {args.length > 0 && (
          <div className="text-muted-foreground/70 truncate font-mono text-xs">
            {t("providers.cli.args", { args: args.join(" ") })}
          </div>
        )}
        {/* Off by default. The setting lives on the provider, so it needs one;
            the switch creates it. */}
        {cli.configured && (
          <label
            htmlFor={`cli-bypass-${cli.protocol}`}
            className="mt-2 flex cursor-pointer items-start gap-2 select-none"
          >
            <Checkbox
              id={`cli-bypass-${cli.protocol}`}
              className="mt-0.5"
              checked={cli.bypass_restrictions}
              disabled={busy}
              onCheckedChange={(v) => onBypass(v === true)}
              aria-label={t("providers.cli.bypass")}
              data-testid={`cli-bypass-${cli.protocol}`}
            />
            <span className="text-xs">
              <span className="text-foreground">
                {t("providers.cli.bypass")}
              </span>
              <span className="text-muted-foreground block">
                {t("providers.cli.bypassHint")}
              </span>
            </span>
          </label>
        )}
        <CLIRegisterLine
          protocol={cli.protocol}
          registration={registration}
          busy={registering}
          onRegister={() => registration && onRegister(registration)}
        />
      </div>

      {/* Shown on every configured row, not only where the switch governs
          several models. Printing it for Claude's three and omitting it for the
          others read as a fault rather than as brevity. */}
      {cli.models > 0 && (
        <span className="text-muted-foreground shrink-0 text-xs">
          {t("providers.cli.modelCount", {
            count: cli.models,
            enabled: cli.models_enabled,
          })}
        </span>
      )}

      {cli.configured && (
        <Button
          variant="ghost"
          size="icon-sm"
          onClick={onEdit}
          title={t("providers.action.edit")}
          data-testid={`cli-edit-${cli.protocol}`}
        >
          <IconEdit className="size-3.5" />
        </Button>
      )}

      {busy ? (
        <IconLoader2 className="text-muted-foreground size-4 shrink-0 animate-spin" />
      ) : (
        <Switch
          checked={cli.enabled}
          disabled={!cli.installed}
          onCheckedChange={onToggle}
          aria-label={cli.label}
          data-testid={`cli-switch-${cli.protocol}`}
        />
      )}
    </div>
  )
}
