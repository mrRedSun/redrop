import { createJiti } from 'jiti'
import { fileURLToPath } from 'node:url'

const entry = {
  test: './unit.test.ts',
  contract: './contract-check.ts',
}[process.argv[2]]
if (!entry) throw new Error('Choose test or contract')

const jiti = createJiti(import.meta.url, {
  alias: { '@': fileURLToPath(new URL('../src', import.meta.url)) },
})
await jiti.import(entry)
