import { describe, expect, it } from "vitest";
import { buildAllowedDevHosts } from "./vite-dev-hosts";

describe("buildAllowedDevHosts", () => {
  it("includes localhost defaults", () => {
    expect(buildAllowedDevHosts()).toEqual(["127.0.0.1", "localhost"]);
  });

  it("adds the public base URL hostname", () => {
    expect(
      buildAllowedDevHosts("https://woozy-unextreme-genny.ngrok-free.dev"),
    ).toEqual([
      "127.0.0.1",
      "localhost",
      "woozy-unextreme-genny.ngrok-free.dev",
    ]);
  });

  it("keeps the ingress target host alongside a public callback host", () => {
    expect(
      buildAllowedDevHosts(
        "https://woozy-unextreme-genny.ngrok-free.dev",
        "platform.localtest.me",
      ),
    ).toEqual([
      "127.0.0.1",
      "localhost",
      "woozy-unextreme-genny.ngrok-free.dev",
      "platform.localtest.me",
    ]);
  });

  it("ignores malformed URLs", () => {
    expect(buildAllowedDevHosts("not-a-url")).toEqual([
      "127.0.0.1",
      "localhost",
      "not-a-url",
    ]);
  });
});
