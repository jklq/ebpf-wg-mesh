export function formatError(
	error: unknown,
	fallback = "An unexpected error occurred.",
): string {
	if (error && typeof error === "object" && "message" in error) {
		return String((error as { message: unknown }).message);
	}
	return fallback;
}
