import { defineConfig, devices } from "@playwright/test";

export default defineConfig({
  testDir: "./tests/browser",
  timeout: 30_000,
  workers: 1,
  use: { trace: "on-first-retry" },
  projects: [
    {
      name: "chromium",
      use: {
        ...devices["Desktop Chrome"],
        launchOptions: {
          args: ["--ip-address-space-overrides=[::1]:0=public,127.0.0.0/8=local"],
        },
      },
    },
  ],
});
