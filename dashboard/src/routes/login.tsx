import { createFileRoute, redirect } from '@tanstack/react-router'
import { createServerFn } from '@tanstack/react-start'

const loadLoginState = createServerFn({ method: 'GET' }).handler(async () => {
  const { listDevLogins, loadDashboardHome } = await import(
    '#/lib/dashboard.server'
  )
  return {
    session: await loadDashboardHome(),
    devUsers: listDevLogins(),
  }
})

export const Route = createFileRoute('/login')({
  validateSearch: (search: Record<string, unknown>) => ({
    redirect: typeof search.redirect === 'string' ? search.redirect : undefined,
  }),
  loader: async () => {
    const state = await loadLoginState()
    if (state.session) {
      throw redirect({ to: '/' })
    }
    return state
  },
  component: LoginPage,
})

function LoginPage() {
  const state = Route.useLoaderData()
  const search = Route.useSearch()

  return (
    <main className="mx-auto flex min-h-screen w-full max-w-3xl items-center px-4 py-8 sm:px-6">
      <section className="w-full rounded-[2rem] border border-[var(--line)] bg-white p-6 shadow-[0_16px_50px_rgba(15,23,42,0.08)] sm:p-10">
        <p className="text-[11px] font-semibold uppercase tracking-[0.24em] text-[var(--accent-strong)]">
          Login
        </p>
        <h1 className="mt-3 text-3xl font-semibold tracking-tight">
          Minimal dashboard entrypoint
        </h1>
        <p className="mt-3 max-w-2xl text-sm text-[var(--muted)]">
          The UI is intentionally sparse. For this slice, the important part is
          that the dashboard terminates the browser session and becomes the only
          user-facing caller of the internal control-plane API.
        </p>

        <div className="mt-8 space-y-3">
          {state.devUsers.length === 0 ? (
            <div className="rounded-2xl border border-dashed border-[var(--line)] bg-[var(--surface-soft)] px-4 py-6 text-sm text-[var(--muted)]">
              No dev login identities are configured. Set
              <code className="ml-1 rounded bg-white px-1.5 py-0.5 text-[var(--ink)]">
                DASHBOARD_DEV_USERS
              </code>
              in the managed service environment.
            </div>
          ) : (
            state.devUsers.map((user) => (
              <a
                key={user.subject}
                href={`/auth/callback?subject=${encodeURIComponent(user.subject)}&email=${encodeURIComponent(user.email)}&redirect=${encodeURIComponent(search.redirect ?? '/')}`}
                className="flex items-center justify-between rounded-2xl border border-[var(--line)] px-4 py-4 transition hover:border-[var(--accent-strong)] hover:bg-[var(--surface-soft)]"
              >
                <div>
                  <p className="font-medium">{user.email}</p>
                  <p className="mt-1 text-xs text-[var(--muted)]">
                    {user.subject}
                  </p>
                </div>
                <span className="text-sm font-medium text-[var(--accent-strong)]">
                  Continue
                </span>
              </a>
            ))
          )}
        </div>
      </section>
    </main>
  )
}
