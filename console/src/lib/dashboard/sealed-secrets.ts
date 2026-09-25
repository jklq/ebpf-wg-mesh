import { z } from "zod";

// Mirrors the control plane's sealed secret rules so input fails before the
// RPC. Messages never echo the value: it must not reach logs or errors.
export const SEALED_SECRET_NAME_MAX_LENGTH = 128;
export const SEALED_SECRET_VALUE_MAX_BYTES = 64 * 1024;

export const sealedSecretName = z
	.string()
	.min(1, "Secret name is required.")
	.max(
		SEALED_SECRET_NAME_MAX_LENGTH,
		`Secret names are at most ${SEALED_SECRET_NAME_MAX_LENGTH} characters.`,
	)
	.regex(
		/^[A-Za-z_][A-Za-z0-9_]*$/,
		"Use letters, digits, and underscores, not starting with a digit.",
	)
	.refine(
		(name) => !name.startsWith("PLATFORM_"),
		"The PLATFORM_ prefix is reserved.",
	);

export const sealedSecretValue = z
	.string()
	.refine(
		(value) => sealedSecretValueBytes(value) <= SEALED_SECRET_VALUE_MAX_BYTES,
		"Secret values are limited to 64 KiB.",
	);

export function sealedSecretValueBytes(value: string): number {
	return new TextEncoder().encode(value).byteLength;
}

/** The first rule a name breaks, or undefined when it is valid. */
export function sealedSecretNameError(name: string): string | undefined {
	return sealedSecretName.safeParse(name).error?.issues[0]?.message;
}
