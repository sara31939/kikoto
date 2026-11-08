import { currentScopedStorageKey, type ClientPrincipalID } from "@/lib/clientStorageScope";

/**
 * Server-side loudness boost (a Sara fork feature). Clients that cannot raise
 * a quiet track themselves — the mobile apps play through the system volume —
 * ask the server to fold the raise into its transcode instead, by adding
 * ?boost=auto to playback requests. The preference is per account and off by
 * default; while it is on, the client applies no loudness gain of its own to
 * local tracks, so a track is never raised twice.
 */
export const PLAYER_SERVER_BOOST_PREFERENCE_CHANGE_EVENT = "kikoto:player-server-boost-preference-change";
const SERVER_BOOST_PREFERENCE_STORAGE_BASE_KEY = "kikoto:player-server-boost:v1";

export type ServerBoostPreferenceChangeDetail = {
  storageKey: string;
  enabled: boolean;
};

export function serverBoostPreferenceStorageKey(principalID: ClientPrincipalID) {
  return currentScopedStorageKey(SERVER_BOOST_PREFERENCE_STORAGE_BASE_KEY, principalID);
}

export function getStoredServerBoost(principalID: ClientPrincipalID): boolean {
  try {
    return localStorage.getItem(serverBoostPreferenceStorageKey(principalID)) === "on";
  } catch {
    return false;
  }
}

export function storeServerBoost(principalID: ClientPrincipalID, enabled: boolean) {
  const storageKey = serverBoostPreferenceStorageKey(principalID);
  try {
    localStorage.setItem(storageKey, enabled ? "on" : "off");
  } catch {
    // The choice still applies to this page when browser storage is unavailable or full.
  }
  window.dispatchEvent(
    new CustomEvent<ServerBoostPreferenceChangeDetail>(PLAYER_SERVER_BOOST_PREFERENCE_CHANGE_EVENT, {
      detail: { storageKey, enabled },
    }),
  );
  return enabled;
}
