import { createJiti } from 'jiti'
import { fileURLToPath } from 'node:url'

const entries = {
  test: ['./unit.test.ts', './viewer-js.test.ts', './viewer-feedback.test.ts', './viewer-live.test.ts', './e2e-crypto.test.ts', './e2e-worker.test.ts', './viewer-e2e.test.ts', './e2e-intake.test.ts', './report-e2e.test.ts'],
  contract: ['./contract-check.ts'],
}[process.argv[2]]
if (!entries) throw new Error('Choose test or contract')

const jiti = createJiti(import.meta.url, {
  alias: { '@': fileURLToPath(new URL('../src', import.meta.url)) },
})
for (const entry of entries) await jiti.import(entry)
