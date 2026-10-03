// Mirrors the control plane's env rules so input fails before the RPC. Messages never echo values.
export const ENV_NAME_MAX_LENGTH = 128;
export const ENV_VALUE_MAX_BYTES = 64 * 1024;
export const RESERVED_ENV_PREFIX = "PLATFORM_";

const ENV_NAME_PATTERN = /^[A-Za-z_][A-Za-z0-9_]*$/;

/** The first rule a variable name breaks, or undefined when it is valid. */
export function envNameError(name: string): string | undefined {
	if (name === "") return "Environment variable name is required.";
	if (name.length > ENV_NAME_MAX_LENGTH || !ENV_NAME_PATTERN.test(name)) {
		return `Invalid environment variable name: ${name}`;
	}
	if (name.startsWith(RESERVED_ENV_PREFIX)) {
		return `The ${RESERVED_ENV_PREFIX} prefix is reserved: ${name}`;
	}
	return undefined;
}

/** The size rule a value breaks, or undefined when it is valid. */
export function envValueError(name: string, value: string): string | undefined {
	if (new TextEncoder().encode(value).byteLength > ENV_VALUE_MAX_BYTES) {
		return `${name} exceeds the 64 KiB value limit.`;
	}
	return undefined;
}
