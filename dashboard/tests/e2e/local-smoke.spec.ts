import { expect, test } from '@playwright/test'

test('login, create project, session persistence, and logout', async ({ page }) => {
  await page.goto('/login')
  await expect(page.getByText('Minimal dashboard entrypoint')).toBeVisible()

  await page.getByRole('link', { name: /continue/i }).click()
  await expect(page.getByText('Authenticated shell')).toBeVisible()
  await expect(page.getByText('Internal PlatformService reachable')).toBeVisible()

  const projectInput = page.getByLabel('Project name')
  await projectInput.click()
  await projectInput.pressSequentially('playwright-demo')
  await expect(projectInput).toHaveValue('playwright-demo')
  await page.getByRole('button', { name: 'Create project' }).click()
  await expect(page.getByText('playwright-demo')).toBeVisible()

  await page.reload()
  await expect(page.getByText('Authenticated shell')).toBeVisible()
  await expect(page.getByText('playwright-demo')).toBeVisible()

  await page.getByRole('link', { name: 'Sign out' }).click()
  await expect(page).toHaveURL(/\/login$/)
})
