// @vitest-environment jsdom

import {
	cleanup,
	fireEvent,
	render,
	screen,
	waitFor,
} from "@testing-library/react";
import { useState } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { DashboardServiceRecord } from "#/lib/dashboard/core/types.server";

import { PanelVariables } from "./panel-variables";

const { doUpdateServiceMock } = vi.hoisted(() => ({
	doUpdateServiceMock: vi.fn(),
}));

vi.mock("./server-fns", () => ({
	doUpdateService: doUpdateServiceMock,
}));

afterEach(() => {
	cleanup();
	doUpdateServiceMock.mockReset();
});

describe("variables panel seed", () => {
	it("opens a blank row for a missing key from a failed build", () => {
		render(
			<PanelVariables
				service={variableService()}
				seedKey="STRIPE_KEY"
				onSaved={() => {}}
			/>,
		);

		const keyInput = screen.getByDisplayValue("STRIPE_KEY");
		const valueInput = document.getElementById(
			keyInput.id.replace(/-key$/, "-value"),
		);
		expect(valueInput).toBeTruthy();
		expect(document.activeElement).toBe(valueInput);
	});

	it("queues variable edits without a save button", async () => {
		doUpdateServiceMock.mockResolvedValue(variableService());
		render(<PanelVariables service={variableService()} onSaved={() => {}} />);

		fireEvent.click(screen.getByRole("button", { name: /add variable/i }));
		const nameInputs = screen.getAllByLabelText("Name");
		const valueInputs = screen.getAllByLabelText("Value");
		fireEvent.change(nameInputs[nameInputs.length - 1], {
			target: { value: "REDIS_URL" },
		});
		fireEvent.change(valueInputs[valueInputs.length - 1], {
			target: { value: "redis://cache" },
		});
		expect(
			nameInputs[nameInputs.length - 1]?.hasAttribute("data-unapplied"),
		).toBe(true);
		expect(
			valueInputs[valueInputs.length - 1]?.hasAttribute("data-unapplied"),
		).toBe(true);
		fireEvent.blur(valueInputs[valueInputs.length - 1]);

		expect(
			screen.queryByRole("button", { name: /save variables/i }),
		).toBeNull();
		await waitFor(() =>
			expect(doUpdateServiceMock).toHaveBeenCalledWith({
				data: {
					serviceId: "service-1",
					runtimeEnv: { REDIS_URL: "redis://cache" },
				},
			}),
		);
	});

	it("does not remount or loop when the parent applies the optimistic env", async () => {
		doUpdateServiceMock.mockResolvedValue(
			variableService({ REDIS_URL: "redis://cache" }),
		);
		function Harness() {
			const [service, setService] = useState(variableService());
			return <PanelVariables service={service} onSaved={setService} />;
		}
		render(<Harness />);

		fireEvent.click(screen.getByRole("button", { name: /add variable/i }));
		const nameInput = screen.getAllByLabelText("Name").at(-1);
		const valueInput = screen.getAllByLabelText("Value").at(-1);
		if (!nameInput || !valueInput) {
			throw new Error("expected a new variable row");
		}
		fireEvent.change(nameInput, { target: { value: "REDIS_URL" } });
		fireEvent.change(valueInput, { target: { value: "redis://cache" } });
		fireEvent.blur(valueInput);

		expect((nameInput as HTMLInputElement).value).toBe("REDIS_URL");
		expect(valueInput).toBe(screen.getAllByLabelText("Value").at(-1));
		expect((valueInput as HTMLInputElement).value).toBe("redis://cache");

		await waitFor(() => expect(doUpdateServiceMock).toHaveBeenCalledTimes(1));
		expect(doUpdateServiceMock).toHaveBeenCalledWith({
			data: {
				serviceId: "service-1",
				runtimeEnv: { REDIS_URL: "redis://cache" },
			},
		});
	});

	it("keeps a raw edit when an older service snapshot arrives during save", async () => {
		const save = deferred<DashboardServiceRecord>();
		doUpdateServiceMock.mockReturnValue(save.promise);
		const onSaved = vi.fn();
		const onSavingChange = vi.fn();
		const initial = variableService({ EXISTING: "one" });
		const { rerender } = render(
			<PanelVariables
				service={initial}
				onSaved={onSaved}
				onSavingChange={onSavingChange}
			/>,
		);

		fireEvent.click(screen.getByRole("button", { name: "Raw" }));
		const editor = screen.getByLabelText("Raw variables");
		fireEvent.change(editor, {
			target: { value: "EXISTING=one\nTOKEN=secret" },
		});

		expect(onSavingChange).not.toHaveBeenCalledWith(true);
		expect(doUpdateServiceMock).not.toHaveBeenCalled();
		fireEvent.click(screen.getByRole("button", { name: "Update variables" }));

		expect(onSavingChange).toHaveBeenLastCalledWith(true);
		expect(
			screen.getByDisplayValue("TOKEN").hasAttribute("data-unapplied"),
		).toBe(true);
		expect(
			screen.getByDisplayValue("secret").hasAttribute("data-unapplied"),
		).toBe(true);
		await waitFor(() => expect(onSavingChange).toHaveBeenLastCalledWith(true));
		await waitFor(() => expect(doUpdateServiceMock).toHaveBeenCalledTimes(1));
		rerender(
			<PanelVariables
				service={variableService({ EXISTING: "stale" })}
				onSaved={onSaved}
				onSavingChange={onSavingChange}
			/>,
		);
		expect(screen.getByDisplayValue("EXISTING")).toBeTruthy();
		expect(screen.getByDisplayValue("one")).toBeTruthy();
		expect(screen.getByDisplayValue("TOKEN")).toBeTruthy();
		expect(screen.getByDisplayValue("secret")).toBeTruthy();

		save.resolve(variableService({ EXISTING: "one", TOKEN: "secret" }));
		await waitFor(() => expect(onSaved).toHaveBeenCalledTimes(1));
		expect(doUpdateServiceMock).toHaveBeenCalledTimes(1);
	});
});

function deferred<T>() {
	let resolve!: (value: T) => void;
	const promise = new Promise<T>((next) => {
		resolve = next;
	});
	return { promise, resolve };
}

function variableService(
	env: Record<string, string> = {},
): DashboardServiceRecord {
	return {
		id: "service-1",
		environmentId: "environment-1",
		projectId: "project-1",
		name: "billing-worker",
		spec: {
			runtime: { env, cpuMillis: 250, memoryMebibytes: 256, ports: [] },
		},
	};
}
