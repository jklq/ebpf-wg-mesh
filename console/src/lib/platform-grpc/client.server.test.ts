import { execFileSync } from "node:child_process";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import {
	constants,
	createSecureServer,
	type ServerHttp2Session,
} from "node:http2";
import { createServer, type Socket } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { Code, ConnectError, createClient } from "@connectrpc/connect";
import { afterAll, beforeAll, expect, it } from "vitest";
import { PlatformService } from "#/lib/platform-gen/platform_pb";
import {
	getTransport,
	isUnprocessed,
	type PlatformRuntimeConfig,
	selectControlPlane,
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

async function replica(refuse = false, owner?: string) {
	let effects = 0;
	let refused = 0;
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
		if (owner) {
			stream.respond({ ":status": 400, "content-type": "application/json" });
			stream.end(
				JSON.stringify({
					code: "failed_precondition",
					message: `not the live owner; reconnect at ${owner}`,
				}),
			);
			return;
		}
		if (refuse) {
			refused++;
			stream.on("error", () => {});
			stream.close(constants.NGHTTP2_REFUSED_STREAM);
			return;
		}
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
		refused: () => refused,
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

it("sends a request refused before processing to the next replica, once per replica", async () => {
	const draining = await replica(true);
	const surviving = await replica();
	const alsoDraining = await replica(true);
	try {
		const runtime: PlatformRuntimeConfig = {
			controlPlaneAddresses: [draining.address, surviving.address],
			controlPlaneServerName: "controlplane.test",
			controlPlaneCA: cert,
			controlPlaneCert: cert,
			controlPlaneKey: key,
			userAssertionSecret: "unused-by-transport",
		};
		const client = createClient(PlatformService, getTransport(runtime));
		await expect(client.createProject({ name: "mutation" })).rejects.toThrow(
			"effect committed",
		);
		expect(draining.refused()).toBe(1);
		expect(surviving.effects()).toBe(1);
		const refusing = createClient(
			PlatformService,
			getTransport({
				...runtime,
				controlPlaneAddresses: [draining.address, alsoDraining.address],
			}),
		);
		await expect(refusing.createProject({ name: "nowhere" })).rejects.toThrow(
			"REFUSED_STREAM",
		);
		expect(draining.refused() + alsoDraining.refused()).toBe(3);
	} finally {
		await draining.close();
		await surviving.close();
		await alsoDraining.close();
	}
});

it("treats only provably unsent failures as unprocessed", () => {
	expect(
		isUnprocessed(
			Object.assign(new Error("connect"), { code: "ECONNREFUSED" }),
		),
	).toBe(true);
	expect(
		isUnprocessed(
			new Error("wrapped", {
				cause: Object.assign(new Error("goaway"), {
					code: "ERR_HTTP2_GOAWAY_SESSION",
				}),
			}),
		),
	).toBe(true);
	expect(
		isUnprocessed(Object.assign(new Error("reset"), { code: "ECONNRESET" })),
	).toBe(false);
	expect(isUnprocessed(new Error("effect committed"))).toBe(false);
	expect(
		isUnprocessed(
			new ConnectError(
				"REFUSED_STREAM after effect committed",
				Code.Unavailable,
			),
		),
	).toBe(false);
});

it("distributes calls across pooled replicas without replaying their effects", async () => {
	const a = await replica();
	const b = await replica();
	try {
		const client = createClient(
			PlatformService,
			getTransport({
				controlPlaneAddresses: [a.address, b.address],
				controlPlaneServerName: "controlplane.test",
				controlPlaneCA: cert,
				controlPlaneCert: cert,
				controlPlaneKey: key,
				userAssertionSecret: "unused-by-transport",
			}),
		);
		for (let n = 0; n < 4; n++) {
			await expect(
				client.createProject({ name: `mutation-${n}` }),
			).rejects.toThrow("effect committed");
		}
		expect(a.effects()).toBe(2);
		expect(b.effects()).toBe(2);
	} finally {
		await a.close();
		await b.close();
	}
});

it("follows a live-owner redirect only to a configured peer before processing", async () => {
	const owner = await replica();
	const follower = await replica(false, owner.address);
	const unknown = await replica(false, "outside-seed-set:9443");
	try {
		const runtime: PlatformRuntimeConfig = {
			controlPlaneAddresses: [follower.address, owner.address],
			controlPlaneServerName: "controlplane.test",
			controlPlaneCA: cert,
			controlPlaneCert: cert,
			controlPlaneKey: key,
			userAssertionSecret: "unused-by-transport",
		};
		await expect(
			createClient(PlatformService, getTransport(runtime)).createProject({
				name: "redirected",
			}),
		).rejects.toThrow("effect committed");
		expect(follower.effects()).toBe(0);
		expect(owner.effects()).toBe(1);
		await expect(
			createClient(
				PlatformService,
				getTransport({
					...runtime,
					controlPlaneAddresses: [unknown.address, owner.address],
				}),
			).createProject({ name: "untrusted-redirect" }),
		).rejects.toThrow("outside-seed-set");
		expect(owner.effects()).toBe(1);
	} finally {
		await follower.close();
		await owner.close();
		await unknown.close();
	}
});

it("bounds a silent replica's connection and avoids it for later requests", async () => {
	const held = new Set<Socket>();
	const silent = createServer((socket) => {
		held.add(socket);
	});
	await new Promise<void>((resolve) => silent.listen(0, "127.0.0.1", resolve));
	const port = (silent.address() as { port: number }).port;
	const surviving = await replica();
	try {
		const runtime: PlatformRuntimeConfig = {
			controlPlaneAddresses: [`127.0.0.1:${port}`, surviving.address],
			controlPlaneServerName: "controlplane.test",
			controlPlaneCA: cert,
			controlPlaneCert: cert,
			controlPlaneKey: key,
			userAssertionSecret: "unused-by-transport",
		};
		const client = createClient(PlatformService, getTransport(runtime));
		const started = Date.now();
		await expect(client.createProject({ name: "first" })).rejects.toThrow(
			"effect committed",
		);
		expect(Date.now() - started).toBeLessThan(4000);
		expect(surviving.effects()).toBe(1);
		const next = Date.now();
		await expect(client.createProject({ name: "second" })).rejects.toThrow(
			"effect committed",
		);
		await expect(client.createProject({ name: "third" })).rejects.toThrow(
			"effect committed",
		);
		expect(Date.now() - next).toBeLessThan(1000);
		expect(surviving.effects()).toBe(3);
	} finally {
		for (const socket of held) socket.destroy();
		await new Promise<void>((resolve) => silent.close(() => resolve()));
		await surviving.close();
	}
}, 15000);
