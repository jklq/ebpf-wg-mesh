import { createFileRoute } from '@tanstack/react-router'

export const Route = createFileRoute('/logout')({
  server: {
    handlers: {
      GET: async () => {
        const { clearSession } = await import('#/lib/dashboard.server')
        await clearSession()
        return new Response(null, {
          status: 302,
          headers: {
            Location: '/login',
          },
        })
      },
    },
  },
  component: LogoutPage,
})

function LogoutPage() {
  return (
    <main className="flex min-h-screen items-center justify-center px-4">
      <div className="rounded-2xl border border-[var(--line)] bg-white px-5 py-4 text-sm text-[var(--muted)]">
        Signing out...
      </div>
    </main>
  )
}
