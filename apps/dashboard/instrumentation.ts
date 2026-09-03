// Boot-time hook. register() runs ONCE per process at server start, before the
// route table is built, which is what makes it the right home for the usage
// poller: the framework ships no scheduler, so an interval started here is the
// only sanctioned place for recurring work.
//
// The poller is gated to production. `webjs dev` would otherwise poll a fleet
// on every reload, and every `createRequestHandler` a test builds would start a
// second one; tests call `startUsagePoller` directly with a fake fetcher
// instead. `PILOT_USAGE_POLL=0` turns it off in production too, which is what a
// second replica would need -- see README.md on why there is only one.
import { setOnError } from '@webjsdev/server';
import { startUsagePoller } from '#modules/usage/poller.server.ts';

export function register() {
  setOnError((error, ctx) => {
    console.error('[instrumentation] request error:', error, ctx ?? '');
  });

  if (process.env.PILOT_USAGE_POLL !== '0' && process.env.NODE_ENV === 'production') {
    startUsagePoller();
  }
}
