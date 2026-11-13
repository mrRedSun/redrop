import type { ReactNode } from 'react'
import { RegistryContext } from '@effect/atom-react'
import { ThemeProvider } from 'next-themes'
import { Toaster } from '@/components/ui/sonner'
import { TooltipProvider } from '@/components/ui/tooltip'
import { registry } from '@/runtime/runtime'

/** The atom registry is the same instance router loaders read (created outside React). */
export function Providers({ children }: { children: ReactNode }) {
  return (
    <ThemeProvider attribute="class" defaultTheme="system" enableSystem disableTransitionOnChange>
      <RegistryContext.Provider value={registry}>
        <TooltipProvider delayDuration={300}>
          {children}
          <Toaster richColors closeButton position="bottom-right" />
        </TooltipProvider>
      </RegistryContext.Provider>
    </ThemeProvider>
  )
}
