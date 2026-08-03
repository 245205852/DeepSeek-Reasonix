import { app } from "../lib/bridge";
import { useT } from "../lib/i18n";
import { useToast } from "../lib/toast";
import type { WorkbenchActiveTarget } from "../lib/workbenchTarget";

type Props = {
  target: WorkbenchActiveTarget;
  onTargetChange: (target: WorkbenchActiveTarget) => void;
};

export default function RemoteReconnectBanner({ target, onTargetChange }: Props) {
  const t = useT();
  const { showToast } = useToast();
  const report = (err: unknown) => showToast(err instanceof Error ? err.message : String(err), "error");

  return (
    <div
      className={`banner banner--actionable ${target.state === "reconnect_failed" ? "banner--error" : "banner--warning"}`}
      role="status"
    >
      <span className="banner__msg">
        {target.state === "reconnecting"
          ? t("remote.banner.reconnecting", { n: target.attempt ?? 1 })
          : t("remote.status.failed")}
        {target.error ? ` ${target.error}` : ""}
      </span>
      <span className="banner__spacer" />
      <button
        type="button"
        className="btn btn--small"
        onClick={() => void app.WorkbenchSwitchLocal().then(onTargetChange).catch(report)}
      >
        {t("remote.switcher.local")}
      </button>
      {target.state === "reconnect_failed" && target.retryable && target.hostId && target.workspace && (
        <button
          type="button"
          className="btn btn--small btn--primary"
          onClick={() => {
            void app.WorkbenchConnectRemote(target.hostId!, target.workspace!)
              .then(async () => onTargetChange(await app.WorkbenchActiveTarget()))
              .catch(report);
          }}
        >
          {t("remote.error.retry")}
        </button>
      )}
    </div>
  );
}
