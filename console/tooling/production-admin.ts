import { readFileSync } from "node:fs";
import { randomUUID } from "node:crypto";
import { Pool } from "pg";
import { migrateDashboardStore } from "../src/lib/dashboard/store/helpers.server";
import { createGitHubTokenCipher, decodeGitHubTokenEncryptionKey } from "../src/lib/dashboard/store/token-crypto.server";
import { createSessionTokenPair, verifyAccessToken, verifyRefreshToken } from "../src/lib/dashboard/core/jwt.server";

// A compiled release tool. Administrative input travels on stdin, never argv.
const input = JSON.parse(readFileSync(0,"utf8")) as {
	databaseURLFile: string;
	schema: string;
	tokenKeyFile: string;
	sessionKeyFile: string;
	installation: string;
	generation: string;
};
if (!/^[a-z][a-z0-9_]*$/.test(input.schema)) throw new Error("invalid console schema");
const db = new Pool({ connectionString: readFileSync(input.databaseURLFile, "utf8").trim(), max: 2 });
const cipher = createGitHubTokenCipher(decodeGitHubTokenEncryptionKey(readFileSync(input.tokenKeyFile,"utf8")));
try {
	if (process.argv[2] === "bootstrap") {
		await migrateDashboardStore({databaseSchema: input.schema,githubTokenCipher:cipher}, db);
	} else if (process.argv[2] === "check") {
		const version = await db.query(`SELECT version FROM ${input.schema}.schema_migrations`);
		if (version.rowCount !== 1 || Number(version.rows[0].version) !== 3) throw new Error("console schema differs from release");
		const user={id:randomUUID(),email:"production-verification@invalid.example"};
		const jwt={installationID:input.installation,recoveryGeneration:input.generation,jwtSecret:readFileSync(input.sessionKeyFile,"utf8").trim()};
		const now=new Date();
		const pair=createSessionTokenPair(jwt,user,randomUUID(),now);
		if(verifyAccessToken(pair.accessToken,jwt,now).user.id!==user.id || verifyRefreshToken(pair.refreshToken,jwt,now).sessionId!==pair.refreshSessionId) throw new Error("console session roundtrip failed");
		const client=await db.connect();
		try {
			await client.query("BEGIN");
			await client.query(`INSERT INTO ${input.schema}.users(id,email,created_at,updated_at) VALUES ($1,$2,now(),now())`,[user.id,user.email]);
			await client.query(`INSERT INTO ${input.schema}.refresh_sessions(id,user_id,created_at,expires_at) VALUES ($1,$2,now(),$3)`,[pair.refreshSessionId,user.id,pair.refreshTokenExpiresAt]);
			const stored=await client.query(`SELECT user_id FROM ${input.schema}.refresh_sessions WHERE id=$1 AND expires_at>now()`,[pair.refreshSessionId]);
			if(stored.rowCount!==1||stored.rows[0].user_id!==user.id) throw new Error("console session persistence failed");
			const binding={userID:user.id,providerSubject:randomUUID(),kind:"access" as const};
			if(cipher.decrypt(cipher.encrypt("verification-token",binding),binding)!=="verification-token") throw new Error("console token key roundtrip failed");
		} finally {await client.query("ROLLBACK");client.release();}
	} else { throw new Error("usage: console-admin <bootstrap|check>"); }
	console.log(JSON.stringify({verified:true}));
} finally {await db.end();}
