import { SITE_ORIGIN } from '#site/lib/links.ts';

/**
 * /robots.txt
 *
 * A metadata route: default-export a function returning a string and the
 * framework serves it as text/plain.
 *
 * Nothing on the marketing site is private, so it is allowed whole. The
 * product shares the origin and is not for a crawler: signed out, `/dashboard`
 * and `/login` answer 200 with a sign-in offer and nothing else, a thin page
 * that would compete with the homepage for the brand query. So is everything
 * under `/oauth` and `/api`. The AI
 * crawlers are named explicitly rather than left to the wildcard, because an
 * infrastructure product is increasingly evaluated by someone asking an
 * assistant what it is before they ever open a search engine. Being readable
 * by the answer engines is a real distribution channel, and naming each agent
 * states the intent unambiguously for tooling that reads per-agent groups.
 */
const AI_CRAWLERS = [
  'ClaudeBot',
  'Claude-SearchBot',
  'Claude-User',
  'GPTBot',
  'OAI-SearchBot',
  'ChatGPT-User',
  'PerplexityBot',
  'Perplexity-User',
  'Google-Extended',
  'Applebot-Extended',
  'CCBot',
  'Amazonbot',
  'meta-externalagent',
  'cohere-ai',
  'YouBot',
];

/** The product's half of the origin. Prefixes, so `/dashboard` covers every page under it. */
const PRODUCT = ['/dashboard', '/login', '/oauth', '/api'];

export default function Robots(): string {
  // A crawler obeys only the most specific group that names it, so the
  // disallows are repeated per agent rather than inherited from the wildcard.
  const rules = ['Allow: /', ...PRODUCT.map((path) => `Disallow: ${path}`), ''];
  const lines = ['User-agent: *', ...rules];
  for (const agent of AI_CRAWLERS) lines.push(`User-agent: ${agent}`, ...rules);
  lines.push(`Sitemap: ${SITE_ORIGIN}/sitemap.xml`, '');
  return lines.join('\n');
}
