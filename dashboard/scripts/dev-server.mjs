import { createServer } from 'vite'

const server = await createServer({
  configFile: 'vite.config.ts',
  logLevel: 'silent',
  server: {
    host: '127.0.0.1',
    port: 0,
  },
})

await server.listen()

const localURLs = server.resolvedUrls?.local ?? []
if (localURLs.length === 0) {
  throw new Error('vite dev server did not expose a local URL')
}

process.stdout.write(`${JSON.stringify({ url: localURLs[0] })}\n`)

const shutdown = async () => {
  await server.close()
  process.exit(0)
}

process.on('SIGINT', shutdown)
process.on('SIGTERM', shutdown)

await new Promise(() => {})
