import { html } from '@webjsdev/core'

export default function Layout({ children }: { children: unknown }) {
  return html`<!doctype html>
    <html lang="en">
      <head>
        <meta charset="utf-8" />
        <meta name="viewport" content="width=device-width, initial-scale=1" />
        <title>webjs on pilots</title>
      </head>
      <body>
        ${children}
      </body>
    </html>`
}
