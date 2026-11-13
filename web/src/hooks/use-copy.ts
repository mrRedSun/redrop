import { useCallback } from 'react'
import { toast } from 'sonner'

/** Copies text to the clipboard and confirms with a toast. */
export function useCopy() {
  return useCallback(async (text: string, label = 'Copied to clipboard') => {
    try {
      await navigator.clipboard.writeText(text)
      toast.success(label)
      return true
    } catch {
      toast.error('Copy failed; select the text and copy it manually')
      return false
    }
  }, [])
}
