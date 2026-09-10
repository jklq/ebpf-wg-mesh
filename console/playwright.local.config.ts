import { defineConfig, devices } from "@playwright/test";

const dashboardBaseURL =
  process.env.DASHBOARD_E2E_BASE_URL ?? "http://platform.localtest.me:8080";
const dashboardReadyURL = new URL("/healthz", dashboardBaseURL).toString();

export default defineConfig({
  testDir: "./tests/e2e",
  timeout: 60_000,
  retries: 0,
  reporter: [
    ["list"],
    ["json", { outputFile: "artifacts/e2e-local/results.json" }],
  ],
  webServer: process.env.DASHBOARD_E2E_BASE_URL
    ? undefined
    : {
        command: "go run ./cmd/localteststack",
        cwd: "..",
        env: {
          LOCALTESTSTACK_RUN_PLAYWRIGHT: "0",
          LOCALTESTSTACK_PRODUCT_E2E: "1",
          LOCALTESTSTACK_ENABLE_PUBLIC_TUNNEL: "1",
          LOCALTESTSTACK_CONSOLE_BIND_ADDRESS: "0.0.0.0",
        },
        url: dashboardReadyURL,
        reuseExistingServer: false,
        timeout: 300_000,
      },
  use: {
    baseURL: dashboardBaseURL,
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
    video: "retain-on-failure",
  },
  projects: [
    {
      name: "chromium",
      use: { ...devices["Desktop Chrome"] },
    },
  ],
});
