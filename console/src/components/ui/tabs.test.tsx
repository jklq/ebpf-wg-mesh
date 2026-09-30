// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { useState } from "react";
import { afterEach, expect, it } from "vitest";
import { TabList } from "./tabs";

afterEach(cleanup);

it("selects and focuses tabs with arrow, Home and End keys and exposes panel relationships", () => {
	function Tabs() {
		const [selected, setSelected] = useState("deployments");
		return (
			<TabList
				id="service-tabs"
				panelId="service-panel"
				label="Service views"
				items={[
					{ id: "deployments", label: "Deployments" },
					{ id: "variables", label: "Variables" },
					{ id: "settings", label: "Settings" },
				]}
				selected={selected}
				onSelect={setSelected}
			/>
		);
	}
	const view = render(<Tabs />);
	const [deployments, variables, settings] = screen.getAllByRole("tab");
	const assertSelected = (tab: HTMLElement) => {
		expect(document.activeElement).toBe(tab);
		expect(tab.getAttribute("aria-selected")).toBe("true");
		expect(tab.tabIndex).toBe(0);
		for (const other of screen.getAllByRole("tab").filter((el) => el !== tab)) {
			expect(other.getAttribute("aria-selected")).toBe("false");
			expect(other.tabIndex).toBe(-1);
		}
	};
	deployments.focus();
	expect(deployments.id).toBe("service-tabs-deployments");
	expect(deployments.getAttribute("aria-controls")).toBe("service-panel");
	fireEvent.keyDown(deployments, { key: "ArrowRight" });
	assertSelected(variables);
	fireEvent.keyDown(variables, { key: "End" });
	assertSelected(settings);
	fireEvent.keyDown(settings, { key: "ArrowRight" });
	assertSelected(deployments);
	fireEvent.keyDown(deployments, { key: "ArrowLeft" });
	assertSelected(settings);
	fireEvent.keyDown(settings, { key: "Home" });
	assertSelected(deployments);
	view.unmount();
});
