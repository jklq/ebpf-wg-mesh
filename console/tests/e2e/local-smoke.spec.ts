import { expect, test } from '@playwright/test'

test('login, create project, session persistence, and logout', async ({ page }) => {
  await page.goto('/login')
  await expect(page.getByText('Minimal dashboard entrypoint')).toBeVisible()

  await page.getByRole('link', { name: /continue/i }).click()
  await expect(page.getByText('Authenticated shell')).toBeVisible()
  await expect(page.getByText('Internal PlatformService reachable')).toBeVisible()

  await expect(page.locator('form[data-hydrated="true"]')).toBeVisible()
  const projectInput = page.getByLabel('Project name')
  const createProjectButton = page.getByRole('button', { name: 'Create project' })
  await projectInput.fill('playwright-demo')
  await expect(projectInput).toHaveValue('playwright-demo')
  await expect(createProjectButton).toBeEnabled()
  await createProjectButton.click()
  await expect(projectInput).toHaveValue('')

  await page.reload()
  await expect(page.getByText('Authenticated shell')).toBeVisible()
  await expect(page.getByText('playwright-demo')).toBeVisible()

  await page.getByRole('link', { name: 'Sign out' }).click()
  await expect(page).toHaveURL(/\/login$/)
})
