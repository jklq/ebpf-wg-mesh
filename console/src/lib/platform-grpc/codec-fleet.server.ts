import type {
	DashboardFleet,
	DashboardFleetAgent,
} from "#/lib/dashboard/core/types.server";
import { decodeAgentLifecycleState } from "./codec-enums.server";
import {
	readArray,
	readBoolean,
	readOptionalDate,
	readOptionalNumberLike,
	readOptionalString,
	readRecord,
	readRequiredString,
	readStringArray,
} from "./codec-read.server";

export function decodeFleetMessage(raw: unknown): DashboardFleet {
	const value = readRecord(raw, "fleet");
	const capacity = readRecord(value.capacity, "fleet capacity");
	return {
		agents: readArray(value, "agents").map(decodeFleetAgentMessage),
		capacity: {
			nodeCount: readOptionalNumberLike(capacity, "nodeCount") ?? 0,
			schedulableNodeCount:
				readOptionalNumberLike(capacity, "schedulableNodeCount") ?? 0,
			schedulableCpuMillis:
				readOptionalNumberLike(capacity, "schedulableCpuMillis") ?? 0,
			schedulableMemoryMebibytes:
				readOptionalNumberLike(capacity, "schedulableMemoryMebibytes") ?? 0,
			allocatedCpuMillis:
				readOptionalNumberLike(capacity, "allocatedCpuMillis") ?? 0,
			allocatedMemoryMebibytes:
				readOptionalNumberLike(capacity, "allocatedMemoryMebibytes") ?? 0,
			headroomCpuMillis:
				readOptionalNumberLike(capacity, "headroomCpuMillis") ?? 0,
			headroomMemoryMebibytes:
				readOptionalNumberLike(capacity, "headroomMemoryMebibytes") ?? 0,
		},
		versionWarning: readOptionalString(value, "versionWarning") || undefined,
	};
}

export function decodeAgentEnrollmentMessage(raw: unknown): {
	agent: DashboardFleetAgent;
	bootstrapToken: string;
} {
	const value = readRecord(raw, "agent enrollment");
	return {
		agent: decodeFleetAgentMessage(value.agent),
		bootstrapToken: readRequiredString(
			value,
			"bootstrapToken",
			"agent enrollment",
		),
	};
}

export function decodeFleetAgentMessage(raw: unknown): DashboardFleetAgent {
	const value = readRecord(raw, "fleet agent");
	return {
		id: readRequiredString(value, "id", "fleet agent"),
		name: readRequiredString(value, "name", "fleet agent"),
		lifecycleState: decodeAgentLifecycleState(value.lifecycleState),
		region: readOptionalString(value, "region") ?? "",
		zone: readOptionalString(value, "zone") ?? "",
		failureDomain: readOptionalString(value, "failureDomain") ?? "",
		healthy: readBoolean(value, "healthy"),
		lastSeenAt: readOptionalDate(value, "lastSeenAt"),
		cpuMillisCapacity: readOptionalNumberLike(value, "cpuMillisCapacity") ?? 0,
		memoryMebibytesCapacity:
			readOptionalNumberLike(value, "memoryMebibytesCapacity") ?? 0,
		reservedCpuMillis: readOptionalNumberLike(value, "reservedCpuMillis") ?? 0,
		reservedMemoryMebibytes:
			readOptionalNumberLike(value, "reservedMemoryMebibytes") ?? 0,
		schedulableCpuMillis:
			readOptionalNumberLike(value, "schedulableCpuMillis") ?? 0,
		schedulableMemoryMebibytes:
			readOptionalNumberLike(value, "schedulableMemoryMebibytes") ?? 0,
		allocatedCpuMillis:
			readOptionalNumberLike(value, "allocatedCpuMillis") ?? 0,
		allocatedMemoryMebibytes:
			readOptionalNumberLike(value, "allocatedMemoryMebibytes") ?? 0,
		headroomCpuMillis: readOptionalNumberLike(value, "headroomCpuMillis") ?? 0,
		headroomMemoryMebibytes:
			readOptionalNumberLike(value, "headroomMemoryMebibytes") ?? 0,
		allocationCount: readOptionalNumberLike(value, "allocationCount") ?? 0,
		runtimeCapabilities: readStringArray(value, "runtimeCapabilities"),
		softwareVersion: readOptionalString(value, "softwareVersion") ?? "",
		versionSkewWarning:
			readOptionalString(value, "versionSkewWarning") || undefined,
		maintenanceMessage:
			readOptionalString(value, "maintenanceMessage") || undefined,
		credentialRevokedAt: readOptionalDate(value, "credentialRevokedAt"),
	};
}
