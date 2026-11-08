import { useTranslation } from "react-i18next";

import { SettingsRow, SettingsSection } from "@/components/settings/SettingsSection";
import { Switch } from "@/components/ui/switch";
import type { ClientPrincipalID } from "@/lib/clientStorageScope";
import { storeServerBoost } from "@/player/serverBoost";
import { useServerBoostEnabled } from "@/player/useServerBoost";

/** Per-account server-side loudness boost (a Sara fork feature); off by default. */
export function PlaybackServerBoostPreference({ userId }: { userId: ClientPrincipalID }) {
  const { t } = useTranslation();
  const enabled = useServerBoostEnabled(userId);
  return (
    <SettingsSection title={t("settings.serverBoost")} description={t("settings.serverBoostDescription")}>
      <SettingsRow title={t("settings.serverBoostRaise")} description={t("settings.serverBoostRaiseDescription")}>
        <Switch
          checked={enabled}
          aria-label={t("settings.serverBoostRaise")}
          onCheckedChange={(checked) => storeServerBoost(userId, checked)}
        />
      </SettingsRow>
    </SettingsSection>
  );
}
