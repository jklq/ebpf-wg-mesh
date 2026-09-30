// @vitest-environment jsdom

import {
	cleanup,
	fireEvent,
	render,
	screen,
	within,
} from "@testing-library/react";
import { useEffect, useRef, useState } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { Dialog } from "./dialog";

afterEach(cleanup);

describe("Dialog", () => {
	it("traps focus, skips hidden controls, and restores the opener and scroll state", () => {
		const opener = document.createElement("button");
		document.body.append(opener);
		opener.focus();
		document.body.style.overflow = "auto";
		const view = render(
			<Dialog label="Edit service">
				<button type="button" disabled>
					Disabled
				</button>
				<div hidden>
					<button type="button">Hidden</button>
				</div>
				<button type="button">First</button>
				<button type="button">Last</button>
			</Dialog>,
		);
		const first = screen.getByRole("button", { name: "First" });
		const last = screen.getByRole("button", { name: "Last" });
		expect(document.activeElement).toBe(first);
		expect(document.body.style.overflow).toBe("hidden");
		fireEvent.keyDown(first, { key: "Tab", shiftKey: true });
		expect(document.activeElement).toBe(last);
		fireEvent.keyDown(last, { key: "Tab" });
		expect(document.activeElement).toBe(first);
		opener.focus();
		expect(document.activeElement).toBe(first);
		view.unmount();
		expect(document.activeElement).toBe(opener);
		expect(document.body.style.overflow).toBe("auto");
		opener.remove();
		document.body.style.overflow = "";
	});

	it("preserves an explicitly focused child and restores the original opener", () => {
		function FocusedInput() {
			const ref = useRef<HTMLInputElement>(null);
			useEffect(() => {
				ref.current?.focus();
			}, []);
			return <input ref={ref} aria-label="Service name" />;
		}
		const opener = document.createElement("button");
		document.body.append(opener);
		opener.focus();
		const view = render(
			<Dialog label="Rename service">
				<button type="button">Cancel</button>
				<FocusedInput />
			</Dialog>,
		);
		expect(document.activeElement).toBe(screen.getByRole("textbox"));
		view.unmount();
		expect(document.activeElement).toBe(opener);
		opener.remove();
	});

	it("closes only the topmost dialog and keeps the parent locked and focused", () => {
		const closeParent = vi.fn();
		function NestedDialog() {
			const [nested, setNested] = useState(false);
			return (
				<Dialog label="Parent" onClose={closeParent}>
					<button type="button" onClick={() => setNested(true)}>
						Open child
					</button>
					{nested && (
						<Dialog label="Child" onClose={() => setNested(false)}>
							<button type="button">Child action</button>
						</Dialog>
					)}
				</Dialog>
			);
		}
		render(<NestedDialog />);
		const opener = screen.getByRole("button", { name: "Open child" });
		fireEvent.click(opener);
		const child = screen.getByRole("dialog", { name: "Child" });
		expect(document.activeElement).toBe(within(child).getByRole("button"));
		fireEvent.click(screen.getByRole("dialog", { name: "Parent" }));
		expect(closeParent).not.toHaveBeenCalled();
		fireEvent.keyDown(document.activeElement as Element, { key: "Escape" });
		expect(screen.queryByRole("dialog", { name: "Child" })).toBeNull();
		expect(closeParent).not.toHaveBeenCalled();
		expect(document.activeElement).toBe(opener);
		expect(document.body.style.overflow).toBe("hidden");
		fireEvent.keyDown(opener, { key: "Escape" });
		expect(closeParent).toHaveBeenCalledTimes(1);
	});

	it("uses the latest close callback and allows required acknowledgement dialogs", () => {
		const firstClose = vi.fn(),
			latestClose = vi.fn();
		const view = render(
			<Dialog label="Dismissable" onClose={firstClose}>
				<span>Content</span>
			</Dialog>,
		);
		view.rerender(
			<Dialog label="Dismissable" onClose={latestClose}>
				<span>Content</span>
			</Dialog>,
		);
		fireEvent.keyDown(document, { key: "Escape" });
		expect(firstClose).not.toHaveBeenCalled();
		expect(latestClose).toHaveBeenCalledTimes(1);
		view.rerender(
			<Dialog label="Save your secret" closeOnBackdrop={false}>
				<span>Content</span>
			</Dialog>,
		);
		fireEvent.keyDown(document, { key: "Escape" });
		fireEvent.click(screen.getByRole("dialog", { name: "Save your secret" }));
		expect(latestClose).toHaveBeenCalledTimes(1);
		expect(
			screen.getByRole("dialog", { name: "Save your secret" }),
		).toBeTruthy();
	});
});
