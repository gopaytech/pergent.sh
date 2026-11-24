// The texts of the two status settings, failure_status and blocking_status,
// shared by a repository's Settings tab and the admin defaults.

export const failureLabel = "Status when a review can't run";
export const blockingLabel = "Status when a review finds blocking issues";
export const failureHelp = `"neutral" doesn't block merging when a review can't be done. On GitLab the status is success; the review is still not approved. Not used when the blocking status is neutral.`;
export const blockingHelp = `"neutral" never blocks merging: pergent's status is never red. The review still lists the blocking issues and is never approved. On GitLab the status is success.`;
// notUsedNote is on the failure status while the blocking status is neutral.
export const notUsedNote = "Not used: the blocking status is neutral";
