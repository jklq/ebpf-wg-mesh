import { Pool, type PoolClient, type PoolConfig } from "pg";

type ConnectCallback = (
	err: Error | undefined,
	client: PoolClient,
	release: (err?: Error | boolean) => void,
) => void;

// Fail over only while obtaining a connection. Once SQL has been submitted its
// result may be ambiguous, so statements and transactions are never replayed.
export class FailoverPool extends Pool {
	private readonly candidates: Pool[];
	private preferred = 0;
	constructor(urls: string[], options: PoolConfig = {}) {
		if (urls.length === 0) throw new Error("database endpoints required");
		super(options);
		this.candidates = urls.map(
			(connectionString) =>
				new Pool({
					...options,
					connectionString,
					connectionTimeoutMillis: 3000,
				}),
		);
		for (const pool of this.candidates)
			pool.on("error", (error) => this.emit("error", error));
	}
	override connect(): Promise<PoolClient>;
	override connect(callback: ConnectCallback): void;
	override connect(callback?: ConnectCallback): Promise<PoolClient> | void {
		const pending = this.connectCandidate();
		if (!callback) return pending;
		pending.then(
			(client) => callback(undefined, client, client.release.bind(client)),
			(error) => callback(error, undefined as unknown as PoolClient, () => {}),
		);
	}
	private async connectCandidate(): Promise<PoolClient> {
		let failure: unknown;
		for (let offset = 0; offset < this.candidates.length; offset++) {
			const index = (this.preferred + offset) % this.candidates.length;
			try {
				const client = await this.candidates[index].connect();
				this.preferred = index;
				return client;
			} catch (error) {
				failure = error;
			}
		}
		throw failure;
	}
	override end(): Promise<void>;
	override end(callback: () => void): void;
	override end(callback?: () => void): Promise<void> | void {
		const pending = Promise.all(this.candidates.map((pool) => pool.end())).then(
			() => super.end(),
		);
		if (!callback) return pending;
		pending.then(callback);
	}
}
