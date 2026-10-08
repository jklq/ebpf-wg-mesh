import { execFileSync } from "node:child_process";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { createSecureServer, type ServerHttp2Session } from "node:http2";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createClient } from "@connectrpc/connect";
import { afterAll, beforeAll, expect, it } from "vitest";
import { PlatformService } from "#/lib/platform-gen/platform_pb";
import {
	getTransport,
	selectControlPlane,
	type PlatformRuntimeConfig,
} from "./client.server";

let directory: string;
let cert: Buffer;
let key: Buffer;
beforeAll(() => {
	directory = mkdtempSync(join(tmpdir(), "console-replicas-"));
	execFileSync(
		"openssl",
		[
			"req",
			"-x509",
			"-nodes",
			"-newkey",
			"rsa:2048",
			"-days",
			"1",
			"-subj",
			"/CN=controlplane.test",
			"-addext",
			"subjectAltName=DNS:controlplane.test",
			"-keyout",
			join(directory, "key.pem"),
			"-out",
			join(directory, "cert.pem"),
		],
		{ stdio: "ignore" },
	);
	cert = readFileSync(join(directory, "cert.pem"));
	key = readFileSync(join(directory, "key.pem"));
});
afterAll(() => rmSync(directory, { recursive: true, force: true }));

async function replica() {
	let effects = 0;
	const sessions = new Set<ServerHttp2Session>();
	const server = createSecureServer({
		cert,
		key,
		ca: cert,
		requestCert: true,
		rejectUnauthorized: true,
	});
	server.on("session", (session) => {
		sessions.add(session);
		session.once("close", () => sessions.delete(session));
	});
	server.on("stream", (stream) => {
		effects++;
		// The mutation has been received, then its result becomes unavailable.
		stream.respond({ ":status": 503, "content-type": "application/json" });
		stream.end(
			JSON.stringify({ code: "unavailable", message: "effect committed" }),
		);
	});
	await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
	const address = server.address();
	if (!address || typeof address === "string")
		throw new Error("native test listener unavailable");
	return {
		address: `127.0.0.1:${address.port}`,
		effects: () => effects,
		close: async () => {
			for (const session of sessions) session.destroy();
			await new Promise<void>((resolve) => server.close(() => resolve()));
		},
	};
}

it("selects an authenticated surviving HTTP/2 replica and never replays a submitted mutation", async () => {
	const a = await replica();
	const b = await replica();
	let firstClosed = false;
	try {
		const runtime: PlatformRuntimeConfig = {
			controlPlaneAddresses: [a.address, b.address],
			controlPlaneServerName: "controlplane.test",
			controlPlaneCA: cert,
			controlPlaneCert: cert,
			controlPlaneKey: key,
			userAssertionSecret: "unused-by-transport",
		};
		expect(await selectControlPlane(runtime)).toBe(a.address);
		const client = createClient(PlatformService, getTransport(runtime));
		await expect(client.createProject({ name: "mutation" })).rejects.toThrow();
		expect(a.effects()).toBe(1);
		expect(b.effects()).toBe(0);
		await a.close();
		firstClosed = true;
		expect(await selectControlPlane(runtime)).toBe(b.address);
		await expect(
			client.createProject({ name: "next-mutation" }),
		).rejects.toThrow();
		expect(b.effects()).toBe(1);
		const aborted = AbortSignal.abort(new Error("cancelled"));
		await expect(selectControlPlane(runtime, aborted)).rejects.toThrow(
			"cancelled",
		);
		expect(b.effects()).toBe(1);
	} finally {
		if (!firstClosed) await a.close();
		await b.close();
	}
});
