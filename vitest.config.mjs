import { defineConfig } from "vitest/config";

export default defineConfig({
  test: {
    setupFiles: ["internal/whiteboard/assets/src/vitest.setup.js"],
  },
});
