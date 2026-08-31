// @vitest-environment jsdom

import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { useAutoQueuedPersist } from "./use-auto-queued-persist";

afterEach(() => {
	cleanup();
	vi.useRealTimers();
});

describe("useAutoQueuedPersist", () => {
	it("persists a changed draft after the debounce window", async () => {
		const persist = vi.fn().mockResolvedValue(undefined);
		const { result, rerender } = renderHook(
			({ incoming, incomingEpoch }) =>
				useAutoQueuedPersist({
					serviceId: "service-1",
					incoming,
					incomingEpoch,
					persist,
					delay: 20,
				}),
			{ initialProps: { incoming: { branch: "main" }, incomingEpoch: 1 } },
		);

		act(() => {
			result.current.setDraft({ branch: "dev" });
		});
		expect(persist).not.toHaveBeenCalled();

		await waitFor(() =>
			expect(persist).toHaveBeenCalledWith({ branch: "dev" }),
		);

		rerender({ incoming: { branch: "dev" }, incomingEpoch: 2 });
		expect(result.current.draft).toEqual({ branch: "dev" });
	});

	it("replaces an acknowledged draft when an external revision arrives", async () => {
		const persist = vi.fn().mockResolvedValue(undefined);
		const { result, rerender } = renderHook(
			({ incoming, incomingEpoch }) =>
				useAutoQueuedPersist({
					serviceId: "service-1",
					incoming,
					incomingEpoch,
					persist,
					delay: 20,
				}),
			{ initialProps: { incoming: { branch: "main" }, incomingEpoch: 1 } },
		);

		rerender({ incoming: { branch: "prod" }, incomingEpoch: 2 });

		await waitFor(() =>
			expect(result.current.draft).toEqual({ branch: "prod" }),
		);
	});

	it("does not let an older subscription revision replace a local edit", async () => {
		const persist = vi.fn().mockResolvedValue(undefined);
		const { result, rerender } = renderHook(
			({ incoming, incomingEpoch }) =>
				useAutoQueuedPersist({
					serviceId: "service-1",
					incoming,
					incomingEpoch,
					persist,
					delay: 20,
				}),
			{ initialProps: { incoming: { branch: "main" }, incomingEpoch: 1 } },
		);

		act(() => {
			result.current.setDraft({ branch: "dev" });
		});
		rerender({ incoming: { branch: "main" }, incomingEpoch: 2 });

		expect(result.current.draft).toEqual({ branch: "dev" });
		await waitFor(() => expect(persist).toHaveBeenCalledTimes(1));
		expect(persist).toHaveBeenCalledWith({ branch: "dev" });
	});

	it("serializes writes for the same service", async () => {
		const first = deferred<void>();
		const persist = vi
			.fn<() => Promise<void>>()
			.mockReturnValueOnce(first.promise)
			.mockResolvedValueOnce(undefined);
		const { result } = renderHook(() =>
			useAutoQueuedPersist({
				serviceId: "service-1",
				incoming: "main",
				persist,
				delay: 1,
			}),
		);

		act(() => result.current.setDraft("dev"));
		await waitFor(() => expect(persist).toHaveBeenCalledTimes(1));
		act(() => result.current.setDraft("release"));
		await new Promise((resolve) => window.setTimeout(resolve, 10));
		expect(persist).toHaveBeenCalledTimes(1);

		first.resolve();
		await waitFor(() => expect(persist).toHaveBeenCalledTimes(2));
		expect(persist).toHaveBeenNthCalledWith(2, "release");
	});
});

function deferred<T>() {
	let resolve!: (value: T) => void;
	const promise = new Promise<T>((next) => {
		resolve = next;
	});
	return { promise, resolve };
}
