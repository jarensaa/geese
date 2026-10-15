// Whether a page's release can still talk to the flock serving it.

// SemVer, with build metadata allowed: a pre-release isn't a release.
const releasePattern = /^v?(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(\+[0-9A-Za-z.-]+)?$/

/** A release's major version, or null for a build that isn't one ("dev", a pre-release). */
export function releaseMajor(release: string): number | null {
  const match = releasePattern.exec(release)
  return match ? Number(match[1]) : null
}

/**
 * Whether a page of this release is older than the oldest the flock serves,
 * as "8.0.0": a breaking release is a major one, so it may call what no server
 * has any more. A build that isn't a release, and a flock that hasn't said,
 * never are.
 */
export function belowOldestPage(release: string, oldestPage: string | null | undefined): boolean {
  const page = releaseMajor(release)
  const oldest = oldestPage ? releaseMajor(oldestPage) : null
  return page !== null && oldest !== null && page < oldest
}

/**
 * Whether an answer could mean the page asked for something the server no
 * longer has, so the page reads the oldest release served again: a request it
 * no longer understands, a path or method it no longer has.
 */
export function unexpectedAnswer(status: number): boolean {
  return status === 400 || status === 404 || status === 405 || status === 410
}
