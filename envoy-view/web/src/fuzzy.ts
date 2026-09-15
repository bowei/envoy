/**
 * Subsequence matching, tuned for xDS names.
 *
 * Names are long, structured, and full of separators — "outbound|8080|v1|
 * svc.ns.svc.cluster.local" — so the useful query is a few characters from the
 * segment you remember. Scoring rewards consecutive characters and matches at
 * a segment boundary, which is what makes "svcl" find "svc.ns...cluster.local"
 * ahead of an incidental scattering of those letters.
 */

const SEPARATORS = new Set(['.', '|', '_', '-', '/', ':', ' '])

const CONSECUTIVE_BONUS = 8
const BOUNDARY_BONUS = 12
const START_BONUS = 16
const GAP_PENALTY = 1

/** Returns a score (higher is better), or null when the query does not match. */
export function fuzzyScore(query: string, text: string): number | null {
  if (query === '') return 0

  const q = query.toLowerCase()
  const t = text.toLowerCase()

  let score = 0
  let ti = 0
  let lastMatch = -2

  for (let qi = 0; qi < q.length; qi++) {
    const want = q[qi]
    let found = -1
    for (; ti < t.length; ti++) {
      if (t[ti] === want) {
        found = ti
        break
      }
    }
    if (found < 0) return null

    if (found === lastMatch + 1) score += CONSECUTIVE_BONUS
    if (found === 0) score += START_BONUS
    else if (SEPARATORS.has(t[found - 1])) score += BOUNDARY_BONUS

    // Skipping a long stretch is weak evidence, but never enough to reject.
    score -= Math.min(found - lastMatch - 1, 10) * GAP_PENALTY

    lastMatch = found
    ti = found + 1
  }

  // Prefer the shorter of two otherwise equal matches: it is more specific.
  return score - t.length / 100
}

export interface Ranked<T> {
  item: T
  score: number
}

export function rank<T>(
  query: string,
  items: T[],
  text: (item: T) => string,
  limit = 20,
): Ranked<T>[] {
  const out: Ranked<T>[] = []
  for (const item of items) {
    const score = fuzzyScore(query, text(item))
    if (score !== null) out.push({ item, score })
  }
  out.sort((a, b) => b.score - a.score)
  return out.slice(0, limit)
}
