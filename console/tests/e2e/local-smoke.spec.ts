import { expect, test } from "@playwright/test";

test("dev login opens the onboarding flow and preserves the session", async ({
	page,
}) => {
	await page.goto("/login");
	await expect(
		page.getByRole("heading", { name: "Continue with GitHub" }),
	).toBeVisible();
	await expect(page.getByText("Development logins")).toBeVisible();

	await page.getByRole("link", { name: /dev@example\.com/i }).click();

	await expect(
		page.getByRole("heading", { name: "Console onboarding" }),
	).toBeVisible();
	await expect(page.getByText("1. Account")).toBeVisible();
	await expect(page.getByText("2. Repository")).toBeVisible();
	await expect(page.getByText("3. Build")).toBeVisible();
	await expect(page.getByText("4. Domain")).toBeVisible();

	const repositoryInput = page.getByPlaceholder("owner/repo");
	await repositoryInput.fill("octocat/hello");
	await expect(repositoryInput).toHaveValue("octocat/hello");

	await page.reload();
	await expect(
		page.getByRole("heading", { name: "Console onboarding" }),
	).toBeVisible();
	await expect(page.getByRole("link", { name: "Sign out" })).toBeVisible();

	await page.getByRole("link", { name: "Sign out" }).click();
	await expect(page).toHaveURL(/\/login$/);
});
