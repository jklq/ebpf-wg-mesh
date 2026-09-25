// @vitest-environment jsdom
import {
	cleanup,
	fireEvent,
	render,
	screen,
	waitFor,
} from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";

const rpc = vi.hoisted(() => ({
	fetchServiceSecrets: vi.fn(),
	doSealServiceSecret: vi.fn(),
	doDeleteServiceSecret: vi.fn(),
}));
vi.mock("./server-fns", () => rpc);

import { PanelSecrets } from "./panel-secrets";

afterEach(cleanup);
it("updates write-only values and keeps backend conflicts actionable", async () => {
	rpc.fetchServiceSecrets.mockResolvedValue([{ name: "TOKEN", version: 2 }]);
	rpc.doSealServiceSecret.mockResolvedValue({ name: "TOKEN", version: 3 });
	render(<PanelSecrets serviceId="service" />);
	await screen.findByText("TOKEN");
	fireEvent.click(screen.getByRole("button", { name: "Update" }));
	const value = screen.getByLabelText("Secret value") as HTMLInputElement;
	expect(value.value).toBe("");
	expect(value.type).toBe("password");
	fireEvent.change(value, { target: { value: "test-only-write" } });
	fireEvent.click(screen.getByRole("button", { name: "Seal secret" }));
	await waitFor(() => expect(value.value).toBe(""));
	expect(rpc.doSealServiceSecret).toHaveBeenCalledWith({
		data: { serviceId: "service", name: "TOKEN", value: "test-only-write" },
	});
	rpc.doSealServiceSecret.mockRejectedValueOnce(
		new Error("Remove the public variable TOKEN first"),
	);
	fireEvent.change(screen.getByLabelText("Secret name"), {
		target: { value: "TOKEN" },
	});
	fireEvent.change(value, { target: { value: "test-only-write" } });
	fireEvent.click(screen.getByRole("button", { name: "Seal secret" }));
	await screen.findByRole("alert");
	expect(screen.getByRole("alert").textContent).toContain(
		"Remove the public variable TOKEN first",
	);
	expect(screen.getByRole("alert").textContent).not.toContain(
		"test-only-write",
	);
});
