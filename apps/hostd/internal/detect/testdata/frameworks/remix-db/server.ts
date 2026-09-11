// A Remix 3 entry point: the app owns the listen call, which is why the
// recipe's notes say server.ts has to read PORT and bind 0.0.0.0.
import { createRequestListener } from 'remix/fetch-proxy'

export default {
  port: Number(process.env.PORT ?? 8080),
  hostname: process.env.HOST ?? '0.0.0.0',
  fetch(request: Request) {
    return new Response('ok')
  },
}
