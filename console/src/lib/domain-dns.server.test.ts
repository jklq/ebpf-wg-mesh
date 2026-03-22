import { describe, expect, it } from "vitest";

import {
  normalizeHostname,
  verifyHostnameDNS,
  type DNSResolver,
} from "#/lib/domain-dns.server";

describe("domain dns verification", () => {
  it("verifies a subdomain CNAME chain", async () => {
    const resolver = fakeResolver({
      cname: {
        "app.example.com": ["edge.platform.example.test"],
        "edge.platform.example.test": ["platform.example.test"],
      },
    });

    await expect(
      verifyHostnameDNS("app.example.com", "platform.example.test", resolver),
    ).resolves.toMatchObject({
      state: "verified",
    });
  });

  it("flags a wrong cname target", async () => {
    const resolver = fakeResolver({
      cname: {
        "app.example.com": ["wrong.example.test"],
      },
    });

    await expect(
      verifyHostnameDNS("app.example.com", "platform.example.test", resolver),
    ).resolves.toMatchObject({
      state: "wrong_target",
    });
  });

  it("verifies an apex by matching A and AAAA records", async () => {
    const resolver = fakeResolver({
      a: {
        "example.com": ["203.0.113.10"],
        "platform.example.test": ["203.0.113.10"],
      },
      aaaa: {
        "example.com": ["2001:db8::10"],
        "platform.example.test": ["2001:db8::10"],
      },
    });

    await expect(
      verifyHostnameDNS("example.com", "platform.example.test", resolver),
    ).resolves.toMatchObject({
      state: "verified",
    });
  });

  it("accepts reserved localteststack hostnames without DNS lookup", async () => {
    const resolver = fakeResolver({});

    await expect(
      verifyHostnameDNS(
        "echo.localtest.me",
        "platform.localtest.me",
        resolver,
        "localtest.me",
      ),
    ).resolves.toMatchObject({
      state: "verified",
      hostname: "echo.localtest.me",
    });
  });

  it("rejects invalid hostnames", () => {
    expect(() => normalizeHostname("https://example.com/path")).toThrow(
      "Enter only a hostname, without a URL scheme.",
    );
    expect(() => normalizeHostname("localhost")).toThrow(
      "Enter a fully qualified hostname.",
    );
    expect(() => normalizeHostname("127.0.0.1")).toThrow(
      "IP addresses are not valid hostnames here.",
    );
  });
});

function fakeResolver(records: {
  cname?: Record<string, Array<string>>;
  a?: Record<string, Array<string>>;
  aaaa?: Record<string, Array<string>>;
}): DNSResolver {
  return {
    async resolveCname(hostname) {
      const values = records.cname?.[hostname];
      if (!values) {
        throw Object.assign(new Error("not found"), { code: "ENOTFOUND" });
      }
      return values;
    },
    async resolve4(hostname) {
      const values = records.a?.[hostname];
      if (!values) {
        throw Object.assign(new Error("not found"), { code: "ENOTFOUND" });
      }
      return values;
    },
    async resolve6(hostname) {
      const values = records.aaaa?.[hostname];
      if (!values) {
        throw Object.assign(new Error("not found"), { code: "ENOTFOUND" });
      }
      return values;
    },
  };
}
