import { defineConfig, devices } from '@playwright/test'

export default defineConfig({
  testDir: './tests/e2e',
  timeout: 30_000,
  retries: 0,
  reporter: [['list'], ['json', { outputFile: 'artifacts/e2e-local/results.json' }]],
  webServer: process.env.DASHBOARD_E2E_BASE_URL
    ? undefined
    : {
        command:
          'cd .. && LOCALTESTSTACK_RUN_PLAYWRIGHT=0 DASHBOARD_DEV_SERVER_PORT=3000 go run ./cmd/localteststack',
        url: 'http://127.0.0.1:3000',
        reuseExistingServer: true,
        timeout: 120_000,
      },
  use: {
    baseURL: process.env.DASHBOARD_E2E_BASE_URL ?? 'http://127.0.0.1:3000',
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
    video: 'retain-on-failure',
  },
  projects: [
    {
      name: 'chromium',
      use: { ...devices['Desktop Chrome'] },
    },
  ],
})
