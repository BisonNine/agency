import { IconLoader2 } from "@tabler/icons-react"
import { Link } from "@tanstack/react-router"
import { useTranslation } from "react-i18next"

import type { CLIRegistration } from "@/api/system"
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog"
import { Button } from "@/components/ui/button"

/**
 * One CLI row's tool server line: whether the tool server is registered in
 * that CLI, and the one button that registers it ("Replace" when the CLI's
 * entry points at another address). While the MCP host is off there is
 * nothing to register: the label says so and links to the setting.
 */
export function CLIRegisterLine({
  protocol,
  registration,
  busy,
  onRegister,
}: {
  protocol: string
  /** Undefined while the states are being read. */
  registration?: CLIRegistration
  busy: boolean
  onRegister: () => void
}) {
  const { t } = useTranslation()
  const state = registration?.state
  const replace = state === "other_address"
  const disabled =
    busy || !state || state === "not_installed" || state === "host_off"

  return (
    <div
      className="mt-2 flex flex-wrap items-center gap-x-2 gap-y-1 text-xs"
      data-testid={`cli-register-${protocol}`}
    >
      <span
        className={
          state === "registered" ? "text-foreground" : "text-muted-foreground"
        }
        data-testid={`cli-register-state-${protocol}`}
        data-state={state ?? "checking"}
      >
        {state === "host_off" ? (
          <Link
            to="/mcp/config"
            className="underline underline-offset-2"
            data-testid={`cli-register-host-off-${protocol}`}
          >
            {t("providers.cli.register.state.host_off")}
          </Link>
        ) : (
          t(`providers.cli.register.state.${state ?? "checking"}`)
        )}
      </span>
      {replace && registration?.address && (
        <span
          className="text-muted-foreground font-mono"
          data-testid={`cli-register-address-${protocol}`}
        >
          {registration.address}
        </span>
      )}
      <Button
        variant="outline"
        size="xs"
        disabled={disabled}
        onClick={onRegister}
        data-testid={`cli-register-button-${protocol}`}
      >
        {busy && <IconLoader2 className="animate-spin" />}
        {replace
          ? t("providers.cli.register.replace")
          : t("providers.cli.register.button")}
      </Button>
    </div>
  )
}

/**
 * The confirmation before an entry pointing at another address is replaced:
 * it names the CLI and both addresses.
 */
export function ReplaceRegistrationDialog({
  target,
  url,
  onCancel,
  onConfirm,
}: {
  target: CLIRegistration | null
  url: string
  onCancel: () => void
  onConfirm: (target: CLIRegistration) => void
}) {
  const { t } = useTranslation()
  return (
    <AlertDialog open={target !== null} onOpenChange={(v) => !v && onCancel()}>
      <AlertDialogContent size="sm" data-testid="cli-register-confirm">
        <AlertDialogHeader>
          <AlertDialogTitle>
            {t("providers.cli.register.confirmTitle")}
          </AlertDialogTitle>
          <AlertDialogDescription>
            {t("providers.cli.register.confirmBody", {
              label: target?.label,
              old: target?.address,
              new: url,
            })}
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel onClick={onCancel}>
            {t("common.cancel")}
          </AlertDialogCancel>
          <AlertDialogAction
            onClick={() => target && onConfirm(target)}
            data-testid="cli-register-confirm-replace"
          >
            {t("providers.cli.register.replace")}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}
