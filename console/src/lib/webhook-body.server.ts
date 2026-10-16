export class WebhookPayloadTooLargeError extends Error {}

/** Reads a webhook body as raw bytes, failing as soon as it exceeds maxBytes. */
export async function readBoundedRequestBody(
	request: Request,
	maxBytes: number,
): Promise<Uint8Array> {
	const contentLength = request.headers.get("content-length");
	if (contentLength !== null) {
		const parsed = Number(contentLength);
		if (!Number.isSafeInteger(parsed) || parsed < 0) {
			throw new Error("invalid content-length");
		}
		if (parsed > maxBytes) {
			throw new WebhookPayloadTooLargeError("webhook payload too large");
		}
	}
	if (!request.body) return new Uint8Array();

	const reader = request.body.getReader();
	const chunks: Uint8Array[] = [];
	let total = 0;
	try {
		for (;;) {
			const { done, value } = await reader.read();
			if (done) break;
			total += value.byteLength;
			if (total > maxBytes) {
				await reader.cancel("webhook payload too large");
				throw new WebhookPayloadTooLargeError("webhook payload too large");
			}
			chunks.push(value);
		}
	} finally {
		reader.releaseLock();
	}

	const payload = new Uint8Array(total);
	let offset = 0;
	for (const chunk of chunks) {
		payload.set(chunk, offset);
		offset += chunk.byteLength;
	}
	return payload;
}
