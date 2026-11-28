/** Sara fork: workflows removed from this build (rankings, subscriptions, catalog sync, remote fetch). */
export const hiddenWorkflowCodes: ReadonlySet<string> = new Set([
  "remote_popular_collection",
  "dlsite_popular_collection",
  "creator_subscription_refresh",
  "circle_catalog_sync",
  "series_catalog_sync",
  "voice_catalog_sync",
  "remote_work_fetch",
]);

export function isHiddenWorkflowCode(code: string): boolean {
  return hiddenWorkflowCodes.has(code);
}
