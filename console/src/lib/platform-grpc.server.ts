import { dirname, resolve } from "node:path";
import * as grpc from "@grpc/grpc-js";
import * as protoLoader from "@grpc/proto-loader";
import googleProtoFiles from "google-proto-files";

import type {
	DashboardProject,
	DashboardUser,
	PlatformGateway,
} from "#/lib/dashboard-core.server";

export interface PlatformRuntimeConfig {
	controlPlaneAddress: string;
	controlPlaneServerName: string;
	controlPlaneCA: Buffer;
	controlPlaneCert: Buffer;
	controlPlaneKey: Buffer;
}

interface EnsurePrincipalRequest {
	subject: string;
	email: string;
}

interface CreateProjectRequest {
	name: string;
}

interface PlatformPrincipalMessage {
	subject: string;
	email: string;
}

interface PlatformProjectMessage {
	id: string;
	name: string;
	kind: DashboardProject["kind"];
	systemKey?: string;
}

interface ListProjectsResponseMessage {
	projects: Array<PlatformProjectMessage>;
}

type PlatformMethod = keyof PlatformRequestMap;

type PlatformRequestMap = {
	EnsurePrincipal: EnsurePrincipalRequest;
	ListProjects: Record<string, never>;
	CreateProject: CreateProjectRequest;
};

type PlatformResponseMap = {
	EnsurePrincipal: PlatformPrincipalMessage;
	ListProjects: ListProjectsResponseMessage;
	CreateProject: PlatformProjectMessage;
};

type RawUnaryCallback = (
	error: grpc.ServiceError | null,
	response: unknown,
) => void;

type PlatformClient = grpc.Client & {
	EnsurePrincipal: (
		request: EnsurePrincipalRequest,
		metadata: grpc.Metadata,
		callback: RawUnaryCallback,
	) => void;
	ListProjects: (
		request: Record<string, never>,
		metadata: grpc.Metadata,
		callback: RawUnaryCallback,
	) => void;
	CreateProject: (
		request: CreateProjectRequest,
		metadata: grpc.Metadata,
		callback: RawUnaryCallback,
	) => void;
};

let clientInstance: PlatformClient | undefined;

export function createPlatformGateway(
	runtime: PlatformRuntimeConfig,
): PlatformGateway {
	return {
		async ensurePrincipal(user): Promise<void> {
			await unaryCall(runtime, "EnsurePrincipal", {
				subject: user.subject,
				email: user.email,
			});
		},
		async listProjects(user): Promise<Array<DashboardProject>> {
			const response = await unaryCall(runtime, "ListProjects", {}, user);
			return response.projects;
		},
		async createProject(user, name): Promise<DashboardProject> {
			return unaryCall(runtime, "CreateProject", { name }, user);
		},
	};
}

export function decodeProjectMessage(raw: unknown): PlatformProjectMessage {
	const value = readRecord(raw, "project");
	return {
		id: readRequiredString(value, "id", "project"),
		name: readRequiredString(value, "name", "project"),
		kind: decodeProjectKind(value.kind),
		systemKey: readOptionalString(value, "systemKey"),
	};
}

export function decodeProjectKind(raw: unknown): DashboardProject["kind"] {
	switch (raw) {
		case "PROJECT_KIND_USER":
		case "user":
			return "user";
		case "PROJECT_KIND_MANAGED":
		case "managed":
			return "managed";
		default:
			throw new Error(`invalid project kind: ${String(raw)}`);
	}
}

function decodePrincipalMessage(raw: unknown): PlatformPrincipalMessage {
	const value = readRecord(raw, "principal");
	return {
		subject: readRequiredString(value, "subject", "principal"),
		email: readRequiredString(value, "email", "principal"),
	};
}

function decodeListProjectsResponse(raw: unknown): ListProjectsResponseMessage {
	const value = readRecord(raw, "list projects response");
	const rawProjects = value.projects;
	if (!Array.isArray(rawProjects)) {
		throw new Error(
			"invalid list projects response: projects must be an array",
		);
	}
	return {
		projects: rawProjects.map((project) => decodeProjectMessage(project)),
	};
}

async function unaryCall<M extends PlatformMethod>(
	runtime: PlatformRuntimeConfig,
	method: M,
	request: PlatformRequestMap[M],
	user?: DashboardUser,
): Promise<PlatformResponseMap[M]> {
	const client = getPlatformClient(runtime);
	const metadata = new grpc.Metadata();
	if (user) {
		metadata.set("x-platform-user-subject", user.subject);
		metadata.set("x-platform-user-email", user.email);
	}
	return new Promise((resolve, reject) => {
		const handleResponse: RawUnaryCallback = (error, response) => {
			if (error) {
				reject(error);
				return;
			}
			try {
				resolve(decodeResponse(method, response));
			} catch (decodeError) {
				reject(decodeError);
			}
		};
		switch (method) {
			case "EnsurePrincipal":
				client.EnsurePrincipal(
					request as EnsurePrincipalRequest,
					metadata,
					handleResponse,
				);
				return;
			case "ListProjects":
				client.ListProjects(
					request as Record<string, never>,
					metadata,
					handleResponse,
				);
				return;
			case "CreateProject":
				client.CreateProject(
					request as CreateProjectRequest,
					metadata,
					handleResponse,
				);
				return;
		}
	});
}

function decodeResponse<M extends PlatformMethod>(
	method: M,
	response: unknown,
): PlatformResponseMap[M] {
	switch (method) {
		case "EnsurePrincipal":
			return decodePrincipalMessage(response) as PlatformResponseMap[M];
		case "ListProjects":
			return decodeListProjectsResponse(response) as PlatformResponseMap[M];
		case "CreateProject":
			return decodeProjectMessage(response) as PlatformResponseMap[M];
	}
}

function getPlatformClient(runtime: PlatformRuntimeConfig): PlatformClient {
	if (clientInstance) {
		return clientInstance;
	}
	const protoPath = resolve(process.cwd(), "../api/proto/platform.proto");
	const definition = protoLoader.loadSync(protoPath, {
		longs: String,
		enums: String,
		defaults: true,
		oneofs: true,
		includeDirs: [
			resolve(process.cwd(), "../api/proto"),
			dirname(googleProtoFiles.getProtoPath()),
		],
	});
	const loaded = grpc.loadPackageDefinition(definition) as {
		platform: {
			v1: {
				PlatformService: grpc.ServiceClientConstructor;
			};
		};
	};
	const credentials = grpc.credentials.createSsl(
		runtime.controlPlaneCA,
		runtime.controlPlaneKey,
		runtime.controlPlaneCert,
	);
	clientInstance = new loaded.platform.v1.PlatformService(
		runtime.controlPlaneAddress,
		grpc.credentials.combineChannelCredentials(
			credentials,
			grpc.credentials.createFromMetadataGenerator((_params, callback) => {
				callback(null, new grpc.Metadata());
			}),
		),
		{
			"grpc.ssl_target_name_override": runtime.controlPlaneServerName,
			"grpc.default_authority": runtime.controlPlaneServerName,
		},
	) as unknown as PlatformClient;
	return clientInstance;
}

function readRecord(raw: unknown, context: string): Record<string, unknown> {
	if (!raw || typeof raw !== "object" || Array.isArray(raw)) {
		throw new Error(`invalid ${context}: expected an object`);
	}
	return raw as Record<string, unknown>;
}

function readRequiredString(
	record: Record<string, unknown>,
	key: string,
	context: string,
): string {
	const value = record[key];
	if (typeof value !== "string" || value === "") {
		throw new Error(`invalid ${context}: ${key} must be a non-empty string`);
	}
	return value;
}

function readOptionalString(
	record: Record<string, unknown>,
	key: string,
): string | undefined {
	const value = record[key];
	if (value === undefined || value === "") {
		return undefined;
	}
	if (typeof value !== "string") {
		throw new Error(`invalid project: ${key} must be a string`);
	}
	return value;
}
