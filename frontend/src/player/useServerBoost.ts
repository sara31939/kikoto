import { useEffect, useState } from "react";

import type { ClientPrincipalID } from "@/lib/clientStorageScope";

import {
  getStoredServerBoost,
  PLAYER_SERVER_BOOST_PREFERENCE_CHANGE_EVENT,
  serverBoostPreferenceStorageKey,
  type ServerBoostPreferenceChangeDetail,
} from "./serverBoost";

/** The account's server-side loudness boost choice, kept in sync with Settings and other tabs. */
export function useServerBoostEnabled(principalID: ClientPrincipalID) {
  const storageKey = serverBoostPreferenceStorageKey(principalID);
  const [enabled, setEnabled] = useState<boolean>(() => getStoredServerBoost(principalID));

  useEffect(() => {
    setEnabled(getStoredServerBoost(principalID));
    const syncCustomEvent = (event: Event) => {
      const detail = (event as CustomEvent<ServerBoostPreferenceChangeDetail>).detail;
      if (detail?.storageKey === storageKey) setEnabled(detail.enabled);
    };
    const syncStorageEvent = (event: StorageEvent) => {
      if (event.key === storageKey) setEnabled(getStoredServerBoost(principalID));
    };
    window.addEventListener(PLAYER_SERVER_BOOST_PREFERENCE_CHANGE_EVENT, syncCustomEvent);
    window.addEventListener("storage", syncStorageEvent);
    return () => {
      window.removeEventListener(PLAYER_SERVER_BOOST_PREFERENCE_CHANGE_EVENT, syncCustomEvent);
      window.removeEventListener("storage", syncStorageEvent);
    };
  }, [principalID, storageKey]);

  return enabled;
}
