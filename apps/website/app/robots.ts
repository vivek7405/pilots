import { SITE_ORIGIN } from '#lib/links.ts';

/**
 * /robots.txt
 *
 * A metadata route: default-export a function returning a string and the
 * framework serves it as text/plain.
 *
 * Nothing on this site is private, so a blanket allow is correct. The AI
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

export default function Robots(): string {
  const lines = ['User-agent: *', 'Allow: /', ''];
  for (const agent of AI_CRAWLERS) lines.push(`User-agent: ${agent}`, 'Allow: /', '');
  lines.push(`Sitemap: ${SITE_ORIGIN}/sitemap.xml`, '');
  return lines.join('\n');
}
