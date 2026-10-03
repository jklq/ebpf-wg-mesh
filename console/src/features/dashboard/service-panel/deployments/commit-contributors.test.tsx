// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, it } from "vitest";
import { CommitContributors } from "#/features/dashboard/service-panel/deployments/commit-contributors";

afterEach(cleanup);

it("stacks avatars author first and collapses the overflow", () => {
	const { container } = render(
		<CommitContributors
			contributors={[
				{ name: "Ada Lovelace", login: "ada", avatarUrl: "https://a/ada.png" },
				{ name: "Grace Hopper", login: "grace", avatarUrl: "https://a/g.png" },
				{ name: "Alan Turing", login: "", avatarUrl: "" },
				{ name: "Linus", login: "linus", avatarUrl: "https://a/l.png" },
			]}
		/>,
	);
	expect(
		screen.getByRole("img", {
			name: "Ada Lovelace, Grace Hopper, Alan Turing, Linus",
		}),
	).toBeTruthy();
	const images = container.querySelectorAll("img");
	expect([...images].map((img) => img.getAttribute("src"))).toEqual([
		"https://a/ada.png",
		"https://a/g.png",
	]);
	expect(screen.getByText("AT")).toBeTruthy();
	expect(screen.getByText("+1")).toBeTruthy();
});

it("falls back to initials when an avatar fails to load", () => {
	const { container } = render(
		<CommitContributors
			contributors={[
				{ name: "Ada Lovelace", login: "", avatarUrl: "https://a/x.png" },
			]}
		/>,
	);
	const img = container.querySelector("img");
	if (!img) throw new Error("avatar not rendered");
	fireEvent.error(img);
	expect(screen.getByText("AL")).toBeTruthy();
});

it("renders nothing without contributors", () => {
	const { container } = render(<CommitContributors contributors={[]} />);
	expect(container.innerHTML).toBe("");
});
