import { readFileSync } from "node:fs";
import { request } from "node:https";
import { randomUUID } from "node:crypto";
import { Pool } from "pg";
import { poolConfigFromURL } from "../src/lib/dashboard/store/pool-config.server";
import { migrateDashboardStore } from "../src/lib/dashboard/store/helpers.server";
import {
	createGitHubTokenCipher,
	decodeGitHubTokenEncryptionKey,
} from "../src/lib/dashboard/store/token-crypto.server";
import {
	createSessionTokenPair,
	verifyAccessToken,
	verifyRefreshToken,
} from "../src/lib/dashboard/core/jwt.server";

// A compiled release tool. Administrative input travels on stdin, never argv.
const input = JSON.parse(readFileSync(0, "utf8")) as {
	databaseURLFile: string;
	schema: string;
	tokenKeyFile: string;
	sessionKeyFile: string;
	installation: string;
	generation: string;
	endpoints?: Array<{ url: string; caFile?: string }>;
	sessionCookieName?: string;
};
if (!/^[a-z][a-z0-9_]*$/.test(input.schema))
	throw new Error("invalid console schema");
const db = new Pool({
	...poolConfigFromURL(readFileSync(input.databaseURLFile, "utf8").trim()),
	max: 2,
});
const cipher = createGitHubTokenCipher(
	decodeGitHubTokenEncryptionKey(readFileSync(input.tokenKeyFile, "utf8")),
);
try {
	if (process.argv[2] === "bootstrap") {
		await migrateDashboardStore(
			{ databaseSchema: input.schema, githubTokenCipher: cipher },
			db,
		);
	} else if (
		process.argv[2] === "check" ||
		process.argv[2] === "check-endpoints"
	) {
		const version = await db.query(
			`SELECT version FROM ${input.schema}.schema_migrations`,
		);
		if (version.rowCount !== 1 || Number(version.rows[0].version) !== 3)
			throw new Error("console schema differs from release");
		const user = {
			id: randomUUID(),
			email: "production-verification@invalid.example",
		};
		const jwt = {
			installationID: input.installation,
			recoveryGeneration: input.generation,
			jwtSecret: readFileSync(input.sessionKeyFile, "utf8").trim(),
		};
		const now = new Date();
		const pair = createSessionTokenPair(jwt, user, randomUUID(), now);
		if (
			verifyAccessToken(pair.accessToken, jwt, now).user.id !== user.id ||
			verifyRefreshToken(pair.refreshToken, jwt, now).sessionId !==
				pair.refreshSessionId
		)
			throw new Error("console session roundtrip failed");
		if (process.argv[2] === "check-endpoints") {
			if (!input.endpoints?.length)
				throw new Error("console endpoints required");
			const inspect = async (
				endpoint: { url: string; caFile?: string },
				token?: string,
			) =>
				new Promise<{ status: number; body: string }>((resolve, reject) => {
					const headers = token
						? {
								cookie: `${input.sessionCookieName ?? "dashboard_session"}=${token}`,
							}
						: {};
					const req = request(
						new URL("/sessionz", endpoint.url),
						{
							method: "GET",
							headers,
							ca: endpoint.caFile ? readFileSync(endpoint.caFile) : undefined,
							rejectUnauthorized: true,
						},
						(response) => {
							let body = "";
							response.setEncoding("utf8");
							response.on("data", (chunk) => {
								body += chunk;
								if (body.length > 65536)
									req.destroy(new Error("oversized console response"));
							});
							response.on("end", () =>
								resolve({ status: response.statusCode ?? 0, body }),
							);
						},
					);
					req.setTimeout(15000, () =>
						req.destroy(new Error("console timeout")),
					);
					req.on("error", reject);
					req.end();
				});
			for (const endpoint of input.endpoints) {
				const accepted = await inspect(endpoint, pair.accessToken);
				if (
					accepted.status !== 200 ||
					JSON.parse(accepted.body).userID !== user.id
				)
					throw new Error("live console session authority differs");
				const rejected = await inspect(endpoint);
				if (rejected.status !== 401)
					throw new Error("live console accepts anonymous sessions");
				const stale = createSessionTokenPair(
					{ ...jwt, recoveryGeneration: "unadmitted-generation" },
					user,
					randomUUID(),
					now,
				);
				if ((await inspect(endpoint, stale.accessToken)).status !== 401)
					throw new Error("live console accepts a stale generation");
			}
		}
		const client = await db.connect();
		try {
			await client.query("BEGIN");
			await client.query(
				`INSERT INTO ${input.schema}.users(id,email,created_at,updated_at) VALUES ($1,$2,now(),now())`,
				[user.id, user.email],
			);
			await client.query(
				`INSERT INTO ${input.schema}.refresh_sessions(id,user_id,created_at,expires_at) VALUES ($1,$2,now(),$3)`,
				[pair.refreshSessionId, user.id, pair.refreshTokenExpiresAt],
			);
			const stored = await client.query(
				`SELECT user_id FROM ${input.schema}.refresh_sessions WHERE id=$1 AND expires_at>now()`,
				[pair.refreshSessionId],
			);
			if (stored.rowCount !== 1 || stored.rows[0].user_id !== user.id)
				throw new Error("console session persistence failed");
			const binding = {
				userID: user.id,
				providerSubject: randomUUID(),
				kind: "access" as const,
			};
			if (
				cipher.decrypt(
					cipher.encrypt("verification-token", binding),
					binding,
				) !== "verification-token"
			)
				throw new Error("console token key roundtrip failed");
		} finally {
			await client.query("ROLLBACK");
			client.release();
		}
	} else {
		throw new Error("usage: console-admin <bootstrap|check>");
	}
	console.log(JSON.stringify({ verified: true }));
} finally {
	await db.end();
}
